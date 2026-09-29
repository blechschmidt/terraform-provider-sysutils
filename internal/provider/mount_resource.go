package provider

import (
	"context"
	"fmt"
	"slices"

	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework-validators/listvalidator"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64default"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/listdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var (
	_ resource.Resource                   = (*mountResource)(nil)
	_ resource.ResourceWithModifyPlan     = (*mountResource)(nil)
	_ resource.ResourceWithConfigure      = (*mountResource)(nil)
	_ resource.ResourceWithImportState    = (*mountResource)(nil)
	_ resource.ResourceWithValidateConfig = (*mountResource)(nil)
)

// maxStackedUnmounts bounds how many file systems stacked on the mount
// point apply unmounts to get to the wanted one.
const maxStackedUnmounts = 16

func NewMountResource() resource.Resource { return &mountResource{} }

type mountResource struct {
	hostOnlyResource
	cfg *mountConfig
}

type mountModel struct {
	Path    types.String `tfsdk:"path"`
	Device  types.String `tfsdk:"device"`
	FSType  types.String `tfsdk:"fstype"`
	Options types.List   `tfsdk:"options"`
	Dump    types.Int64  `tfsdk:"dump"`
	Pass    types.Int64  `tfsdk:"pass"`
	Persist types.Bool   `tfsdk:"persist"`
	Mounted types.Bool   `tfsdk:"mounted"`
	ID      types.String `tfsdk:"id"`
}

func (r *mountResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_mount"
}

func (r *mountResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	defaultOptions := make([]attr.Value, len(defaultMountOptions))
	for i, o := range defaultMountOptions {
		defaultOptions[i] = types.StringValue(o)
	}
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages a file system mount and, optionally, its entry in `/etc/fstab`. " +
			"Mounts with `mount(8)` and unmounts with `umount(8)`, and edits `/etc/fstab` atomically, keeping comments and all other entries. " +
			"On destroy the file system is unmounted and its `/etc/fstab` entry removed. Requires root privileges.",
		Attributes: map[string]schema.Attribute{
			"path": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "Absolute path of the mount point. It must already exist, as a directory or, for bind mounts, a file, and no component of it may be a symlink. " +
					"Must be in canonical form (no `.`/`..` segments, duplicate or trailing slashes) and must not be `/`. Changing this forces a new resource.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
				Validators:    []validator.String{absolutePath()},
			},
			"device": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "What to mount, as in the first field of `/etc/fstab`: a block device such as `\"/dev/sdb1\"`, a tag such as `\"UUID=...\"` or `\"LABEL=...\"`, " +
					"a network share such as `\"server:/export\"`, the source directory of a bind mount, or any name for virtual file systems such as `\"tmpfs\"`.",
				Validators: []validator.String{stringCheck("device", validateMountDevice)},
			},
			"fstype": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "File system type, as passed to `mount -t` and written to the third field of `/etc/fstab`, such as `\"ext4\"`, `\"xfs\"`, `\"nfs\"` or `\"tmpfs\"`. " +
					"Use `\"none\"` for bind mounts.",
				Validators: []validator.String{stringCheck("file system type", validateFSType)},
			},
			"options": schema.ListAttribute{
				ElementType: types.StringType,
				Optional:    true,
				Computed:    true,
				Default:     listdefault.StaticValue(types.ListValueMust(types.StringType, defaultOptions)),
				MarkdownDescription: "Mount options, one per element, such as `[\"noatime\", \"nodev\"]` or `[\"bind\"]`. " +
					"Written comma-separated to the fourth field of `/etc/fstab`. When the options change on a mounted file system, it is remounted with `mount -o remount`. " +
					"Defaults to `[\"defaults\"]`.",
				Validators: []validator.List{
					listvalidator.SizeAtLeast(1),
					listvalidator.ValueStringsAre(stringCheck("mount option", validateMountOption)),
				},
			},
			"dump": schema.Int64Attribute{
				Optional:            true,
				Computed:            true,
				Default:             int64default.StaticInt64(0),
				MarkdownDescription: "The fifth field of `/etc/fstab`, used by `dump(8)`. Defaults to `0`.",
				Validators:          []validator.Int64{int64validator.AtLeast(0)},
			},
			"pass": schema.Int64Attribute{
				Optional: true,
				Computed: true,
				Default:  int64default.StaticInt64(0),
				MarkdownDescription: "The sixth field of `/etc/fstab`: the order in which `fsck(8)` checks the file system at boot. " +
					"`0` skips the check, `1` is for the root file system and `2` for all others. Defaults to `0`.",
				Validators: []validator.Int64{int64validator.Between(0, 9)},
			},
			"persist": schema.BoolAttribute{
				Optional: true,
				Computed: true,
				Default:  booldefault.StaticBool(true),
				MarkdownDescription: "Whether the mount has an entry in `/etc/fstab`, so that it is mounted again at boot. " +
					"If `true`, the entry for `path` is added or updated; if `false`, any entry for `path` is removed. Defaults to `true`.",
			},
			"mounted": schema.BoolAttribute{
				Optional: true,
				Computed: true,
				Default:  booldefault.StaticBool(true),
				MarkdownDescription: "Whether the file system is mounted now. If `false`, it is unmounted, and with `persist = true` only the `/etc/fstab` entry is managed. " +
					"At least one of `persist` and `mounted` must be `true`. Defaults to `true`.",
			},
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Resource identifier (equal to `path`).",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
		},
	}
}

func (r *mountResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if req.ProviderData == nil {
		return // Not configured yet, e.g. during validation.
	}
	data, ok := req.ProviderData.(*providerData)
	if !ok {
		resp.Diagnostics.AddError("Unexpected provider data", fmt.Sprintf("Expected *providerData, got %T.", req.ProviderData))
		return
	}
	r.cfg = data.mount
	r.fsRoot = data.root
}

func (r *mountResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var persist, mounted types.Bool
	resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root("persist"), &persist)...)
	resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root("mounted"), &mounted)...)
	if resp.Diagnostics.HasError() {
		return
	}
	isFalse := func(v types.Bool) bool { return !v.IsNull() && !v.IsUnknown() && !v.ValueBool() }
	if isFalse(persist) && isFalse(mounted) {
		resp.Diagnostics.AddAttributeError(path.Root("mounted"), "Invalid configuration",
			"At least one of persist and mounted must be true; with both false there is nothing to manage.")
	}
}

func (r *mountResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan mountModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	target := plan.Path.ValueString()
	if err := validateAbsolutePath(target); err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("path"), "Invalid path", capitalize(err.Error())+".")
		return
	}

	// Refuse to take over a mount or an fstab entry that already exists:
	// destroying the resource would unmount it or remove the entry.
	fstab := r.cfg.fstab()
	t, _, err := readFstab(fstab)
	if err != nil {
		resp.Diagnostics.AddError("Reading fstab", capitalize(err.Error())+".")
		return
	}
	if e, _ := lookupFstabEntry(t, target); e != nil {
		resp.Diagnostics.AddAttributeError(path.Root("path"), "Mount point already in fstab",
			fmt.Sprintf("%s already has an entry for %q (%s). Import it with \"terraform import\" instead, or remove the entry.", fstab, target, e.String()))
		return
	}
	live, err := lookupMount(r.cfg.mnt(), target)
	if err != nil {
		resp.Diagnostics.AddError("Reading mount table", capitalize(err.Error())+".")
		return
	}
	if live != nil {
		resp.Diagnostics.AddAttributeError(path.Root("path"), "Already mounted",
			fmt.Sprintf("%s (type %s) is already mounted at %q. Import it with \"terraform import\" instead, or unmount it.", live.source, live.fstype, target))
		return
	}

	plan.ID = types.StringValue(target)
	changed, diags := r.apply(ctx, &plan, nil)
	resp.Diagnostics.Append(diags...)
	// After a partial apply, record the resource so that it is tainted and
	// destroy can clean up what was done.
	if !resp.Diagnostics.HasError() || changed {
		resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
	}
}

func (r *mountResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state mountModel
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

func (r *mountResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state mountModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	plan.ID = types.StringValue(plan.Path.ValueString())
	_, diags := r.apply(ctx, &plan, &state)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *mountResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state mountModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	target := state.Path.ValueString()
	// State is not validated by the schema; never act on an invalid path.
	if err := validateAbsolutePath(target); err != nil {
		resp.Diagnostics.AddError("Refusing to unmount", capitalize(err.Error())+".")
		return
	}
	m := r.cfg.mnt()
	live, err := lookupMount(m, target)
	if err != nil {
		resp.Diagnostics.AddError("Reading mount table", capitalize(err.Error())+".")
		return
	}
	// Unmount first: if that fails, the fstab entry is kept and matches the
	// state that remains.
	if live != nil {
		if err := m.unmount(ctx, target); err != nil {
			resp.Diagnostics.AddError("Unmounting", capitalize(err.Error())+".")
			return
		}
	}
	fstab := r.cfg.fstab()
	if err := editFstab(fstab, func(t *textFile) bool { return removeFstabEntries(t, target) }); err != nil {
		resp.Diagnostics.AddError("Editing fstab", fmt.Sprintf("Removing the entry for %q from %s: %s.", target, fstab, err))
	}
}

// ImportState imports a mount by its mount point. The fstab entry, if any,
// provides device, fstype, options, dump and pass; otherwise they are taken
// from the mount table.
func (r *mountResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	if err := validateAbsolutePath(req.ID); err != nil {
		resp.Diagnostics.AddError("Invalid import ID", fmt.Sprintf("Import ID must be the absolute path of the mount point: %s.", err))
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("path"), req.ID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), req.ID)...)
}

// apply brings the mount and the fstab entry in line with plan. prev is the
// prior state on update and nil on create. It reports whether the host may
// have been changed, so that a failed create can still be recorded.
func (r *mountResource) apply(ctx context.Context, plan, prev *mountModel) (changed bool, diags diag.Diagnostics) {
	target := plan.Path.ValueString()
	if err := validateAbsolutePath(target); err != nil {
		diags.AddAttributeError(path.Root("path"), "Invalid path", capitalize(err.Error())+".")
		return false, diags
	}
	want, d := plan.entry(ctx)
	diags.Append(d...)
	if diags.HasError() {
		return false, diags
	}
	m := r.cfg.mnt()
	live, err := lookupMount(m, target)
	if err != nil {
		diags.AddError("Reading mount table", capitalize(err.Error())+".")
		return false, diags
	}

	// Mount before writing fstab, so that a device or options that cannot be
	// mounted fail the apply before they can break the next boot.
	if plan.Mounted.ValueBool() {
		optionsChanged := false
		if prev != nil {
			prevEntry, d := prev.entry(ctx)
			diags.Append(d...)
			if diags.HasError() {
				return false, diags
			}
			optionsChanged = !slices.Equal(prevEntry.options, want.options)
		}
		// Unmount whatever else is mounted on top, such as a file system
		// mounted out of band or the old device after a change of device.
		// That may uncover the wanted file system mounted below it.
		for i := 0; live != nil && !mountMatches(want, live); i++ {
			if i == maxStackedUnmounts {
				diags.AddError("Unmounting", fmt.Sprintf("Still no matching file system at %q after unmounting %d others.", target, i))
				return changed, diags
			}
			if err := m.unmount(ctx, target); err != nil {
				diags.AddError("Unmounting", fmt.Sprintf("Unmounting %s (type %s) from %q to mount %s instead: %s.", live.source, live.fstype, target, want.device, err))
				return changed, diags
			}
			changed = true
			if live, err = lookupMount(m, target); err != nil {
				diags.AddError("Reading mount table", capitalize(err.Error())+".")
				return changed, diags
			}
		}
		switch {
		case live == nil:
			if err := checkMountPoint(target); err != nil {
				diags.AddAttributeError(path.Root("path"), "Invalid mount point", capitalize(err.Error())+".")
				return changed, diags
			}
			if err := m.mount(ctx, want.device, target, want.fstype, want.options); err != nil {
				diags.AddError("Mounting", capitalize(err.Error())+".")
				return changed, diags
			}
			changed = true
		case optionsChanged || readOnlyDrift(want.options, live):
			if err := m.remount(ctx, want.device, target, want.options); err != nil {
				diags.AddError("Remounting", fmt.Sprintf("Applying the new options to %q: %s. Some options can only be changed by unmounting and mounting again.", target, err))
				return changed, diags
			}
			changed = true
		}
	} else if live != nil {
		if err := m.unmount(ctx, target); err != nil {
			diags.AddError("Unmounting", capitalize(err.Error())+".")
			return false, diags
		}
		changed = true
	}

	fstab := r.cfg.fstab()
	err = editFstab(fstab, func(t *textFile) bool {
		if plan.Persist.ValueBool() {
			return setFstabEntry(t, want)
		}
		return removeFstabEntries(t, target)
	})
	if err != nil {
		diags.AddError("Editing fstab", fmt.Sprintf("Updating the entry for %q in %s: %s.", target, fstab, err))
		return changed, diags
	}

	// Verify the result, so that a mount(8) that reported success without
	// mounting, or a mount the kernel reports differently, fails now instead
	// of showing up as a permanent diff.
	live, err = lookupMount(m, target)
	if err != nil {
		diags.AddError("Reading mount table", capitalize(err.Error())+".")
		return changed, diags
	}
	switch {
	case plan.Mounted.ValueBool() && live == nil:
		diags.AddError("Mounting", fmt.Sprintf("Nothing is mounted at %q after mounting it.", target))
	case plan.Mounted.ValueBool() && !mountMatches(want, live):
		diags.AddError("Mounting", fmt.Sprintf("After mounting, the mount table reports %s (type %s) at %q instead of %s (type %s).",
			live.source, live.fstype, target, want.device, want.fstype))
	case !plan.Mounted.ValueBool() && live != nil:
		diags.AddError("Unmounting", fmt.Sprintf("%s (type %s) is still mounted at %q after unmounting it; another file system may be stacked below it.",
			live.source, live.fstype, target))
	}
	return true, diags
}

// refresh updates m from fstab and the mount table. device, fstype, options,
// dump and pass come from the fstab entry if there is one. If the mounted
// file system certainly differs from the one in m, its device or type is
// recorded instead, so that the plan shows the difference; see mountMatches.
func (r *mountResource) refresh(ctx context.Context, m *mountModel) diag.Diagnostics {
	var diags diag.Diagnostics
	target := m.Path.ValueString()
	if err := validateAbsolutePath(target); err != nil {
		diags.AddError("Invalid state", capitalize(err.Error())+".")
		return diags
	}
	imported := m.Device.IsNull()
	// What was last applied, to compare the mount table against. After
	// import, the fstab entry takes that role.
	var prior *fstabEntry
	if !imported {
		e, d := m.entry(ctx)
		diags.Append(d...)
		if diags.HasError() {
			return diags
		}
		prior = &e
	}

	fstab := r.cfg.fstab()
	t, _, err := readFstab(fstab)
	if err != nil {
		diags.AddError("Reading fstab", capitalize(err.Error())+".")
		return diags
	}
	entry, count := lookupFstabEntry(t, target)
	live, err := lookupMount(r.cfg.mnt(), target)
	if err != nil {
		diags.AddError("Reading mount table", capitalize(err.Error())+".")
		return diags
	}
	if imported && entry == nil && live == nil {
		diags.AddError("Cannot import mount",
			fmt.Sprintf("Nothing is mounted at %q and %s has no entry for it.", target, fstab))
		return diags
	}
	if count > 1 {
		diags.AddWarning("Duplicate fstab entries",
			fmt.Sprintf("%s has %d entries for %q. Only the first one is used; the next apply that changes this resource removes the others.", fstab, count, target))
	}

	m.ID = types.StringValue(target)
	m.Persist = types.BoolValue(entry != nil)
	m.Mounted = types.BoolValue(live != nil)
	if entry != nil {
		diags.Append(m.setEntry(ctx, *entry)...)
		if prior == nil {
			prior = entry
		}
	}
	if live == nil {
		return diags
	}
	if prior == nil {
		// Imported, and only mounted: start from the mount table. Its
		// options include kernel defaults that were never configured, so
		// only the read-only flag is carried over.
		opts := defaultMountOptions
		if live.readOnly() {
			opts = []string{"ro"}
		}
		diags.Append(m.setEntry(ctx, fstabEntry{device: live.source, mountPoint: target, fstype: live.fstype, options: opts})...)
		return diags
	}
	if !fstypeMatches(prior.fstype, live.fstype) && !isBindMount(prior.options) {
		m.FSType = types.StringValue(live.fstype)
	}
	if deviceDiffers(prior.device, prior.fstype, prior.options, live) {
		m.Device = types.StringValue(live.source)
	}
	if readOnlyDrift(prior.options, live) {
		opts, d := m.options(ctx)
		diags.Append(d...)
		for i, o := range opts {
			if o == "ro" {
				opts[i] = "rw"
			}
		}
		diags.Append(m.setOptions(ctx, opts)...)
	}
	return diags
}

// mountMatches reports whether live can be the mount described by want.
func mountMatches(want fstabEntry, live *mountEntry) bool {
	if !isBindMount(want.options) && !fstypeMatches(want.fstype, live.fstype) {
		return false
	}
	return !deviceDiffers(want.device, want.fstype, want.options, live)
}

// readOnlyDrift reports whether options ask for a read-only mount but live is
// writable. The opposite is not reported: the kernel mounts write-protected
// media read-only even without "ro".
func readOnlyDrift(options []string, live *mountEntry) bool {
	return slices.Contains(options, "ro") && !live.readOnly()
}

// entry returns the fstab entry described by m.
func (m *mountModel) entry(ctx context.Context) (fstabEntry, diag.Diagnostics) {
	opts, diags := m.options(ctx)
	return fstabEntry{
		device:     m.Device.ValueString(),
		mountPoint: m.Path.ValueString(),
		fstype:     m.FSType.ValueString(),
		options:    opts,
		dump:       m.Dump.ValueInt64(),
		pass:       m.Pass.ValueInt64(),
	}, diags
}

func (m *mountModel) setEntry(ctx context.Context, e fstabEntry) diag.Diagnostics {
	m.Device = types.StringValue(e.device)
	m.FSType = types.StringValue(e.fstype)
	m.Dump = types.Int64Value(e.dump)
	m.Pass = types.Int64Value(e.pass)
	return m.setOptions(ctx, e.options)
}

func (m *mountModel) options(ctx context.Context) ([]string, diag.Diagnostics) {
	if m.Options.IsNull() || m.Options.IsUnknown() {
		return slices.Clone(defaultMountOptions), nil
	}
	var opts []string
	diags := m.Options.ElementsAs(ctx, &opts, false)
	return opts, diags
}

func (m *mountModel) setOptions(ctx context.Context, opts []string) diag.Diagnostics {
	v, diags := types.ListValueFrom(ctx, types.StringType, opts)
	m.Options = v
	return diags
}

// stringCheckValidator adapts a validation function to validator.String.
type stringCheckValidator struct {
	what  string
	check func(string) error
}

func stringCheck(what string, check func(string) error) validator.String {
	return stringCheckValidator{what: what, check: check}
}

func (v stringCheckValidator) Description(_ context.Context) string {
	return "value must be a valid " + v.what
}

func (v stringCheckValidator) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}

func (v stringCheckValidator) ValidateString(_ context.Context, req validator.StringRequest, resp *validator.StringResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	if err := v.check(req.ConfigValue.ValueString()); err != nil {
		resp.Diagnostics.AddAttributeError(req.Path, "Invalid "+v.what, capitalize(err.Error())+".")
	}
}
