package provider

import (
	"context"
	"fmt"

	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var (
	_ resource.Resource                   = (*packageResource)(nil)
	_ resource.ResourceWithConfigure      = (*packageResource)(nil)
	_ resource.ResourceWithModifyPlan     = (*packageResource)(nil)
	_ resource.ResourceWithValidateConfig = (*packageResource)(nil)
	_ resource.ResourceWithImportState    = (*packageResource)(nil)
)

func NewPackageResource() resource.Resource { return &packageResource{} }

type packageResource struct {
	hostOnlyResource
	cfg *packageConfig
}

type packageModel struct {
	Name             types.String `tfsdk:"name"`
	Version          types.String `tfsdk:"version"`
	State            types.String `tfsdk:"state"`
	Manager          types.String `tfsdk:"manager"`
	UpdateCache      types.Bool   `tfsdk:"update_cache"`
	RemoveOnDestroy  types.Bool   `tfsdk:"remove_on_destroy"`
	InstalledVersion types.String `tfsdk:"installed_version"`
	ID               types.String `tfsdk:"id"`
}

func (r *packageResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_package"
}

func (r *packageResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Installs, upgrades, pins to a version or removes an OS package with the host's package manager: `apt-get` on Debian and Ubuntu, `dnf` or `yum` on Fedora and RHEL-like distributions, `apk` on Alpine. " +
			"The installed version is read from the package database (`dpkg-query`, `rpm -q`, `apk info`), so packages installed, upgraded or removed outside Terraform show up as drift. " +
			"Commands run without a shell and without prompts. Requires root privileges.",
		Attributes: map[string]schema.Attribute{
			"name": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "Name of the package, such as `\"nginx\"`. 1 to 128 letters, digits, `.`, `_`, `+` or `-`, starting with a letter or digit; with apt, lowercase and at least 2 characters, as Debian requires. " +
					"Architecture qualifiers, version constraints, globs, file paths and virtual `provides` are not accepted. Changing this forces a new resource.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
				Validators:    []validator.String{stringCheck("package name", validatePackageName)},
			},
			"version": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "Exact version to install, upgrading or downgrading the installed package to it, in the form the package database reports: " +
					"`[epoch:]upstream[-revision]` for apt (`\"2.10-3build1\"`), `[epoch:]version-release` for dnf and yum (`\"2.2.1-1.fc42\"`; the release is required), `version-rN` for apk (`\"2.2.1-r0\"`). " +
					"Only allowed with `state = \"present\"`. If unset, any installed version is accepted.",
				Validators: []validator.String{stringCheck("package version", validatePackageVersion)},
			},
			"state": schema.StringAttribute{
				Optional: true,
				Computed: true,
				Default:  stringdefault.StaticString(packageStatePresent),
				MarkdownDescription: "`\"present\"` installs the package if it is missing (the newest available version, unless `version` is set); " +
					"`\"latest\"` also upgrades it whenever the package index has a newer version; `\"absent\"` removes it. Defaults to `\"present\"`.",
				Validators: []validator.String{stringvalidator.OneOf(packageStatePresent, packageStateLatest, packageStateAbsent)},
			},
			"manager": schema.StringAttribute{
				Optional: true,
				Computed: true,
				Default:  stringdefault.StaticString(packageManagerAuto),
				MarkdownDescription: "Package manager to use: `\"apt\"`, `\"dnf\"`, `\"yum\"` or `\"apk\"`. " +
					"`\"auto\"` uses the first of them whose tools are installed, in that order. Defaults to `\"auto\"`.",
				Validators: []validator.String{stringvalidator.OneOf(packageManagerAuto, packageManagerApt, packageManagerDnf, packageManagerYum, packageManagerApk)},
			},
			"update_cache": schema.BoolAttribute{
				Optional: true,
				Computed: true,
				Default:  booldefault.StaticBool(false),
				MarkdownDescription: "During apply, refresh the package index (`apt-get update`, `dnf makecache`, `yum makecache`, `apk update`) before installing or upgrading the package, and always before checking for upgrades with `state = \"latest\"`; refresh during plan never updates it. " +
					"The index is refreshed at most once per provider run, however many resources set this. Needed on fresh container images, which ship without an index. Defaults to `false`.",
			},
			"remove_on_destroy": schema.BoolAttribute{
				Optional: true,
				Computed: true,
				Default:  booldefault.StaticBool(true),
				MarkdownDescription: "Whether destroying the resource removes the package, if `state` is `\"present\"` or `\"latest\"`. " +
					"Set to `false` for packages that must stay installed, such as ones that were installed before Terraform managed them. Defaults to `true`.",
			},
			"installed_version": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Version of the installed package, as reported by the package database; null if the package is not installed.",
			},
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Resource identifier (equal to `name`).",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
		},
	}
}

func (r *packageResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	data, diags := providerDataFrom(req.ProviderData)
	resp.Diagnostics.Append(diags...)
	if data != nil {
		r.cfg = data.pkg
		r.fsRoot = data.root
	}
}

func (r *packageResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var m packageModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if !m.Version.IsNull() && !m.State.IsUnknown() && !m.State.IsNull() && m.State.ValueString() != packageStatePresent {
		resp.Diagnostics.AddAttributeError(path.Root("version"), "Invalid version",
			fmt.Sprintf("version can only be set with state = %q, not %q.", packageStatePresent, m.State.ValueString()))
	}
	// With an explicit manager, its stricter rules can be checked at plan
	// time; with "auto" they are checked once the manager is known.
	if m.Manager.IsNull() || m.Manager.IsUnknown() || m.Manager.ValueString() == packageManagerAuto {
		return
	}
	kind := m.Manager.ValueString()
	if !m.Name.IsNull() && !m.Name.IsUnknown() && validatePackageName(m.Name.ValueString()) == nil {
		if err := validatePackageNameFor(kind, m.Name.ValueString()); err != nil {
			resp.Diagnostics.AddAttributeError(path.Root("name"), "Invalid package name", capitalize(err.Error())+".")
		}
	}
	if !m.Version.IsNull() && !m.Version.IsUnknown() && validatePackageVersion(m.Version.ValueString()) == nil {
		if err := validatePackageVersionFor(kind, m.Version.ValueString()); err != nil {
			resp.Diagnostics.AddAttributeError(path.Root("version"), "Invalid package version", capitalize(err.Error())+".")
		}
	}
}

// ModifyPlan refuses root_dir and predicts installed_version where it can:
// null for an absent package, the configured version if one is set, and
// the current value if the package is left alone.
func (r *packageResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	r.hostOnlyResource.ModifyPlan(ctx, req, resp)
	if resp.Diagnostics.HasError() || req.Plan.Raw.IsNull() {
		return
	}
	var plan packageModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	var prior *packageModel
	if !req.State.Raw.IsNull() {
		prior = &packageModel{}
		resp.Diagnostics.Append(req.State.Get(ctx, prior)...)
		if resp.Diagnostics.HasError() {
			return
		}
	}
	installed := types.StringUnknown()
	switch {
	case plan.State.IsUnknown() || plan.Version.IsUnknown():
	case plan.State.ValueString() == packageStateAbsent:
		installed = types.StringNull()
	case !plan.Version.IsNull():
		installed = plan.Version
	case prior != nil && prior.State.Equal(plan.State) && prior.Name.Equal(plan.Name) && !prior.InstalledVersion.IsNull():
		// Refresh found the package as wanted, so apply will not touch it.
		installed = prior.InstalledVersion
	}
	resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("installed_version"), installed)...)
}

func (r *packageResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan packageModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	plan.ID = types.StringValue(plan.Name.ValueString())
	changed, diags := r.apply(ctx, &plan)
	resp.Diagnostics.Append(diags...)
	// After a failed install or removal the package may be half there.
	// Record the resource, so that it is tainted and destroy can clean up.
	if !resp.Diagnostics.HasError() || changed {
		resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
	}
}

func (r *packageResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state packageModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(r.refresh(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *packageResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan packageModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	plan.ID = types.StringValue(plan.Name.ValueString())
	changed, diags := r.apply(ctx, &plan)
	resp.Diagnostics.Append(diags...)
	if !resp.Diagnostics.HasError() || changed {
		resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
	}
}

func (r *packageResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state packageModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if state.State.ValueString() == packageStateAbsent || (!state.RemoveOnDestroy.IsNull() && !state.RemoveOnDestroy.ValueBool()) {
		return
	}
	name := state.Name.ValueString()
	cfg := r.cfg.orDefault()
	// State is not validated by the schema; never pass an invalid name to
	// the package manager.
	mgr, diags := r.manager(cfg, &state)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	cfg.mu.Lock()
	defer cfg.mu.Unlock()
	info, err := mgr.Query(ctx, name)
	if err != nil {
		resp.Diagnostics.AddError("Querying package", capitalize(err.Error())+".")
		return
	}
	if !info.Installed {
		return
	}
	if err := mgr.Remove(ctx, name); err != nil {
		resp.Diagnostics.AddError("Removing package", capitalize(err.Error())+".")
		return
	}
	resp.Diagnostics.Append(verifyPackageRemoved(ctx, mgr, name)...)
}

// ImportState imports an installed package by name, with state "present"
// and no version.
func (r *packageResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	if err := validatePackageName(req.ID); err != nil {
		resp.Diagnostics.AddError("Invalid import ID", fmt.Sprintf("Import ID must be a package name: %s.", err))
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("name"), req.ID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), req.ID)...)
}

// manager validates m's name and version for the configured manager and
// returns that manager.
func (r *packageResource) manager(cfg *packageConfig, m *packageModel) (packageManager, diag.Diagnostics) {
	var diags diag.Diagnostics
	name := m.Name.ValueString()
	if err := validatePackageName(name); err != nil {
		diags.AddAttributeError(path.Root("name"), "Invalid package name", capitalize(err.Error())+".")
		return nil, diags
	}
	kind := packageManagerAuto
	if !m.Manager.IsNull() {
		kind = m.Manager.ValueString()
	}
	mgr, err := cfg.resolve(kind)
	if err != nil {
		diags.AddAttributeError(path.Root("manager"), "Package manager not available", capitalize(err.Error())+".")
		return nil, diags
	}
	if err := validatePackageNameFor(mgr.Kind(), name); err != nil {
		diags.AddAttributeError(path.Root("name"), "Invalid package name", capitalize(err.Error())+".")
	}
	if !m.Version.IsNull() {
		if err := validatePackageVersionFor(mgr.Kind(), m.Version.ValueString()); err != nil {
			diags.AddAttributeError(path.Root("version"), "Invalid package version", capitalize(err.Error())+".")
		}
	}
	if diags.HasError() {
		return nil, diags
	}
	return mgr, diags
}

// apply brings the package in line with plan and sets its
// installed_version. It reports whether a command that changes the host
// was run, so that a failed create can still be recorded.
func (r *packageResource) apply(ctx context.Context, plan *packageModel) (changed bool, diags diag.Diagnostics) {
	cfg := r.cfg.orDefault()
	mgr, diags := r.manager(cfg, plan)
	if diags.HasError() {
		return false, diags
	}
	name, version, state := plan.Name.ValueString(), plan.Version.ValueString(), plan.State.ValueString()
	if state != packageStatePresent && version != "" {
		diags.AddAttributeError(path.Root("version"), "Invalid version", fmt.Sprintf("version can only be set with state = %q.", packageStatePresent))
		return false, diags
	}

	cfg.mu.Lock()
	defer cfg.mu.Unlock()
	info, err := mgr.Query(ctx, name)
	if err != nil {
		diags.AddError("Querying package", capitalize(err.Error())+".")
		return false, diags
	}
	updateCache := func() bool {
		if !plan.UpdateCache.ValueBool() {
			return true
		}
		if err := cfg.updateCacheOnce(ctx, mgr); err != nil {
			diags.AddAttributeError(path.Root("update_cache"), "Updating package index", capitalize(err.Error())+".")
			return false
		}
		return true
	}

	switch state {
	case packageStateAbsent:
		if info.Installed {
			changed = true
			if err := mgr.Remove(ctx, name); err != nil {
				diags.AddError("Removing package", capitalize(err.Error())+".")
			}
		}
	case packageStatePresent:
		if !info.Installed || (version != "" && !samePackageVersion(info.Version, version)) {
			if !updateCache() {
				return false, diags
			}
			changed = true
			if err := mgr.Install(ctx, name, version); err != nil {
				diags.AddError("Installing package", capitalize(err.Error())+".")
			}
		}
	case packageStateLatest:
		if !updateCache() {
			return false, diags
		}
		upToDate := false
		if info.Installed {
			if upToDate, err = mgr.UpToDate(ctx, name); err != nil {
				diags.AddError("Checking for package upgrades", capitalize(err.Error())+".")
				return false, diags
			}
		}
		if !upToDate {
			changed = true
			if err := mgr.Upgrade(ctx, name); err != nil {
				diags.AddError("Upgrading package", capitalize(err.Error())+".")
			}
		}
	default:
		diags.AddAttributeError(path.Root("state"), "Invalid state", fmt.Sprintf("Unsupported state %q.", state))
		return false, diags
	}
	// Record what the package manager left behind, even after a failure.
	after, err := mgr.Query(ctx, name)
	if err != nil {
		diags.AddError("Querying package", capitalize(err.Error())+".")
		plan.InstalledVersion = types.StringNull()
		return changed, diags
	}
	plan.InstalledVersion = installedVersionValue(after, plan.Version)
	if !diags.HasError() {
		diags.Append(verifyPackageState(ctx, mgr, name, version, state, after)...)
	}
	return changed, diags
}

// verifyPackageState checks the outcome of an apply. Package managers can
// report success without doing what was asked: apt-get installs a package
// that provides a virtual name instead, and a held or excluded package is
// silently left alone by some of them.
func verifyPackageState(ctx context.Context, mgr packageManager, name, version, state string, info packageInfo) (diags diag.Diagnostics) {
	switch state {
	case packageStateAbsent:
		if info.Installed {
			diags.AddError("Package not removed",
				fmt.Sprintf("%s reported success, but %s %s is still installed. Another package may require it.", mgr.Kind(), name, info.Version))
		}
		return diags
	}
	if !info.Installed {
		diags.AddError("Package not installed",
			fmt.Sprintf("%s reported success, but the package database does not list %s as installed. The name may be a virtual package or an alias that another package provides; use the name of the real package instead.", mgr.Kind(), name))
		return diags
	}
	if version != "" && !samePackageVersion(info.Version, version) {
		diags.AddAttributeError(path.Root("version"), "Package version not installed",
			fmt.Sprintf("%s reported success, but %s %s is installed instead of %s. The package may be held or excluded from changes.", mgr.Kind(), name, info.Version, version))
	}
	if state == packageStateLatest {
		upToDate, err := mgr.UpToDate(ctx, name)
		if err != nil {
			diags.AddError("Checking for package upgrades", capitalize(err.Error())+".")
		} else if !upToDate {
			diags.AddAttributeError(path.Root("state"), "Package not upgraded",
				fmt.Sprintf("%s reported success, but a newer version of %s than %s is still available. The package may be held or excluded from upgrades.", mgr.Kind(), name, info.Version))
		}
	}
	return diags
}

func verifyPackageRemoved(ctx context.Context, mgr packageManager, name string) diag.Diagnostics {
	info, err := mgr.Query(ctx, name)
	if err != nil {
		var diags diag.Diagnostics
		diags.AddError("Querying package", capitalize(err.Error())+".")
		return diags
	}
	return verifyPackageState(ctx, mgr, name, "", packageStateAbsent, info)
}

// installedVersionValue returns the installed_version for info. A version
// that equals the configured one except for an explicit zero epoch is
// reported as configured, so that the plan's prediction holds.
func installedVersionValue(info packageInfo, configured types.String) types.String {
	if !info.Installed {
		return types.StringNull()
	}
	if !configured.IsNull() && !configured.IsUnknown() && samePackageVersion(info.Version, configured.ValueString()) {
		return configured
	}
	return types.StringValue(info.Version)
}

// refresh updates m from the package database. state is set to what is
// found on the host, so that drift shows up as a change of state: "absent"
// if the package is not installed, and "present" instead of "latest" if an
// upgrade is available. A different installed version shows up as a change
// of version, if one is configured.
func (r *packageResource) refresh(ctx context.Context, m *packageModel) (diags diag.Diagnostics) {
	cfg := r.cfg.orDefault()
	imported := m.State.IsNull()
	if imported {
		m.Manager = types.StringValue(packageManagerAuto)
		m.UpdateCache = types.BoolValue(false)
		m.RemoveOnDestroy = types.BoolValue(true)
		m.Version = types.StringNull()
	}
	mgr, diags := r.manager(cfg, m)
	if diags.HasError() {
		return diags
	}
	name := m.Name.ValueString()

	cfg.mu.Lock()
	defer cfg.mu.Unlock()
	info, err := mgr.Query(ctx, name)
	if err != nil {
		diags.AddError("Querying package", capitalize(err.Error())+".")
		return diags
	}
	m.ID = types.StringValue(name)
	m.InstalledVersion = installedVersionValue(info, m.Version)

	switch {
	case imported && !info.Installed:
		diags.AddError("Cannot import package", fmt.Sprintf("Package %s is not installed according to %s.", name, mgr.Kind()))
		return diags
	case imported:
		m.State = types.StringValue(packageStatePresent)
	case !info.Installed:
		m.State = types.StringValue(packageStateAbsent)
	case m.State.ValueString() == packageStateLatest:
		upToDate, err := mgr.UpToDate(ctx, name)
		if err != nil {
			diags.AddError("Checking for package upgrades", capitalize(err.Error())+".")
			return diags
		}
		if !upToDate {
			m.State = types.StringValue(packageStatePresent)
		}
	default:
		m.State = types.StringValue(packageStatePresent)
	}
	if info.Installed && !m.Version.IsNull() && !samePackageVersion(info.Version, m.Version.ValueString()) {
		m.Version = types.StringValue(info.Version)
	}
	return diags
}
