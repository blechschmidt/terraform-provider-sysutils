package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework-validators/mapvalidator"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var (
	_ resource.Resource                = (*localeResource)(nil)
	_ resource.ResourceWithConfigure   = (*localeResource)(nil)
	_ resource.ResourceWithImportState = (*localeResource)(nil)
	_ resource.ResourceWithModifyPlan  = (*localeResource)(nil)
)

// localePrivatePrevious is the private state key of what the locale file
// assigned before the first apply, as a JSON localeFileState.
const localePrivatePrevious = "previous"

func NewLocaleResource() resource.Resource { return &localeResource{} }

type localeResource struct {
	rootedResource
	cfg *localeConfig
}

type localeModel struct {
	Lang             types.String `tfsdk:"lang"`
	LC               types.Map    `tfsdk:"lc"`
	Path             types.String `tfsdk:"path"`
	Generate         types.Bool   `tfsdk:"generate"`
	RestoreOnDestroy types.Bool   `tfsdk:"restore_on_destroy"`
	ID               types.String `tfsdk:"id"`
}

func (r *localeResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_locale"
}

func (r *localeResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Sets the system locale: `LANG` and optionally `LC_*` variables in `/etc/locale.conf` (systemd-based distributions such as Fedora, RHEL and Arch) or `/etc/default/locale` (Debian, Ubuntu). " +
			"Other lines of the file are kept. With `generate = true`, locales that are not installed are compiled with `locale-gen` or `localedef`. " +
			"There is one system locale, so there must be at most one `sysutils_locale` per host (or per `root_dir`). " +
			"On destroy the locale stays as it is, unless `restore_on_destroy` is set.",
		Attributes: map[string]schema.Attribute{
			"lang": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "Value of `LANG`, the default locale for every category, such as `\"en_US.UTF-8\"` or `\"C.UTF-8\"`. " +
					"Letters, digits, `_`, `.`, `@`, `+` and `-`.",
				Validators: []validator.String{stringCheck("locale", validateLocaleName)},
			},
			"lc": schema.MapAttribute{
				ElementType: types.StringType,
				Optional:    true,
				MarkdownDescription: "Locales of individual categories, by variable name, such as `{ LC_TIME = \"en_GB.UTF-8\", LC_PAPER = \"de_DE.UTF-8\" }`. " +
					"Keys must be `" + strings.Join(localeLCVariables, "`, `") + "`; `LC_ALL` is not supported. " +
					"Every `LC_*` variable of the file that is not listed here is removed.",
				Validators: []validator.Map{
					mapvalidator.KeysAre(stringCheck("locale category variable", validateLCVariable)),
					mapvalidator.ValueStringsAre(stringCheck("locale", validateLocaleName)),
				},
			},
			"path": schema.StringAttribute{
				Optional: true,
				Computed: true,
				MarkdownDescription: "Absolute path of the locale file, inside the provider's `root_dir` if set. " +
					"Detected if not set: `/etc/default/locale` if it is a regular file, else `/etc/locale.conf` if it exists, else `/etc/default/locale` if `/etc/debian_version` exists, else `/etc/locale.conf`. " +
					"The file is created with mode `0644` if it does not exist, together with its directory. A symlink at `path` is refused. " +
					"Changing this forces a new resource.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown(), stringplanmodifier.RequiresReplace()},
				Validators:    []validator.String{absolutePath()},
			},
			"generate": schema.BoolAttribute{
				Optional: true,
				Computed: true,
				Default:  booldefault.StaticBool(false),
				MarkdownDescription: "Whether to compile the locales of `lang` and `lc` that `locale -a` does not list, when applying. " +
					"Where `/etc/locale.gen` and `locale-gen` exist (Debian, Ubuntu, Arch), the locale is enabled in `/etc/locale.gen` and `locale-gen` runs; otherwise `localedef` compiles it (Fedora and RHEL need `glibc-locale-source`). " +
					"Not supported with the provider's `root_dir`. Defaults to `false`; missing locales then only cause a warning.",
			},
			"restore_on_destroy": schema.BoolAttribute{
				Optional: true,
				Computed: true,
				Default:  booldefault.StaticBool(false),
				MarkdownDescription: "Whether destroy puts back the `LANG` and `LC_*` assignments that the file had before the resource was created, and removes the file if it did not exist. " +
					"They are recorded in the resource's private state on create; after import there is nothing to restore, and destroy only warns. " +
					"Defaults to `false`: destroy leaves the locale as it is.",
			},
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Always `\"" + singletonID + "\"`.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
		},
	}
}

func (r *localeResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	data, diags := providerDataFrom(req.ProviderData)
	resp.Diagnostics.Append(diags...)
	if data != nil {
		r.cfg = data.locale
		r.fsRoot = data.root
	}
}

// ModifyPlan refuses generate with root_dir: the locale tools act on the
// host, not on the tree below root_dir.
func (r *localeResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.Plan.Raw.IsNull() || r.root().isHost() {
		return
	}
	var generate types.Bool
	resp.Diagnostics.Append(req.Plan.GetAttribute(ctx, path.Root("generate"), &generate)...)
	if generate.ValueBool() {
		resp.Diagnostics.AddAttributeError(path.Root("generate"), "Not supported with root_dir",
			fmt.Sprintf("locale-gen and localedef compile locales for the running host, not for the tree below root_dir = %q. "+
				"Generate the locale in the tree otherwise, for example by installing a locale package into it, and set generate = false.", r.root().String()))
	}
}

// vars returns the managed variables that m assigns.
func (m *localeModel) vars(ctx context.Context) (map[string]string, diag.Diagnostics) {
	vars := map[string]string{}
	var diags diag.Diagnostics
	if !m.LC.IsNull() && !m.LC.IsUnknown() {
		diags.Append(m.LC.ElementsAs(ctx, &vars, false)...)
		if diags.HasError() {
			return nil, diags
		}
	}
	vars[localeLangVar] = m.Lang.ValueString()
	// Config validation already enforces this; re-check as defense in depth,
	// since the values are written to a file that shells source.
	for k, v := range vars {
		if k != localeLangVar {
			if err := validateLCVariable(k); err != nil {
				diags.AddAttributeError(path.Root("lc"), "Invalid variable", capitalize(err.Error())+".")
			}
		}
		if err := validateLocaleName(v); err != nil {
			diags.AddAttributeError(path.Root("lang"), "Invalid locale", capitalize(err.Error())+".")
		}
	}
	return vars, diags
}

// resolvePath returns the configured or detected locale file, and its host
// path.
func (r *localeResource) resolvePath(m *localeModel) (p, host string, diags diag.Diagnostics) {
	p = m.Path.ValueString()
	if m.Path.IsNull() || m.Path.IsUnknown() || p == "" {
		var err error
		if p, err = detectLocalePath(r.root()); err != nil {
			diags.AddAttributeError(path.Root("path"), "Detecting locale file", capitalize(err.Error())+".")
			return "", "", diags
		}
	}
	if err := validateAbsolutePath(p); err != nil {
		diags.AddAttributeError(path.Root("path"), "Invalid path", capitalize(err.Error())+".")
		return "", "", diags
	}
	host, d := resolvePathAttr(r.root(), p, false)
	diags.Append(d...)
	return p, host, diags
}

func (r *localeResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan localeModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	p, host, diags := r.resolvePath(&plan)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	// Record what the file assigns now, before anything is changed, so
	// that destroy can put it back.
	prev, err := readLocaleFileState(host)
	if err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("path"), "Reading locale file", capitalize(err.Error())+".")
		return
	}
	raw, err := json.Marshal(prev)
	if err != nil {
		resp.Diagnostics.AddError("Encoding private state", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.Private.SetKey(ctx, localePrivatePrevious, raw)...)
	if resp.Diagnostics.HasError() {
		return
	}

	plan.Path = types.StringValue(p)
	resp.Diagnostics.Append(r.apply(ctx, &plan, host)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *localeResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan localeModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	p, host, diags := r.resolvePath(&plan)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	plan.Path = types.StringValue(p)
	resp.Diagnostics.Append(r.apply(ctx, &plan, host)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// apply compiles missing locales if requested and writes the locale file at
// host.
func (r *localeResource) apply(ctx context.Context, plan *localeModel, host string) diag.Diagnostics {
	vars, diags := plan.vars(ctx)
	if diags.HasError() {
		return diags
	}
	names := make([]string, 0, len(vars))
	names = append(names, vars[localeLangVar])
	for _, k := range slices.Sorted(maps.Keys(vars)) {
		if k != localeLangVar {
			names = append(names, vars[k])
		}
	}
	if r.root().isHost() {
		diags.Append(r.ensureInstalled(ctx, names, plan.Generate.ValueBool())...)
		if diags.HasError() {
			return diags
		}
	} else if plan.Generate.ValueBool() {
		diags.AddAttributeError(path.Root("generate"), "Not supported with root_dir", "generate = true cannot be used with the provider's root_dir.")
		return diags
	}
	if err := writeLocaleVars(host, vars, false); err != nil {
		diags.AddAttributeError(path.Root("path"), "Writing locale file", capitalize(err.Error())+".")
		return diags
	}
	plan.ID = types.StringValue(singletonID)
	return diags
}

// ensureInstalled checks that the locales in names are installed on the
// host, compiling missing ones if generate is set and warning about them
// otherwise.
func (r *localeResource) ensureInstalled(ctx context.Context, names []string, generate bool) diag.Diagnostics {
	var diags diag.Diagnostics
	installed, err := r.cfg.available(ctx)
	if err != nil {
		if generate {
			diags.AddAttributeError(path.Root("generate"), "Listing installed locales", capitalize(err.Error())+".")
		}
		// Without generate, the check is only advisory; musl-based
		// systems such as Alpine have no locale command.
		return diags
	}
	missing := missingLocales(installed, names)
	if len(missing) == 0 {
		return diags
	}
	if !generate {
		diags.AddWarning("Locale not installed",
			fmt.Sprintf("locale -a does not list %s. Programs fall back to the C locale until it is installed. Set generate = true to compile it, or install the distribution's language pack.", strings.Join(missing, ", ")))
		return diags
	}
	if err := r.cfg.generate(ctx, missing); err != nil {
		diags.AddAttributeError(path.Root("generate"), "Generating locales", capitalize(err.Error())+".")
		return diags
	}
	if installed, err = r.cfg.available(ctx); err != nil {
		diags.AddAttributeError(path.Root("generate"), "Listing installed locales", capitalize(err.Error())+".")
		return diags
	}
	if still := missingLocales(installed, names); len(still) > 0 {
		diags.AddAttributeError(path.Root("generate"), "Locale not generated",
			fmt.Sprintf("After generating, locale -a still does not list %s. Check that the name is a locale glibc supports.", strings.Join(still, ", ")))
	}
	return diags
}

func (r *localeResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state localeModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	imported := state.Lang.IsNull()
	p, host, diags := r.resolvePath(&state)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	cur, err := readLocaleFileState(host)
	if err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("path"), "Reading locale file", capitalize(err.Error())+".")
		return
	}
	lang, hasLang := cur.Vars[localeLangVar]
	if imported && !hasLang {
		resp.Diagnostics.AddError("Cannot import locale", fmt.Sprintf("%s does not set %s.", p, localeLangVar))
		return
	}
	state.Path = types.StringValue(p)
	state.Lang = types.StringNull()
	if hasLang {
		state.Lang = types.StringValue(strings.ToValidUTF8(lang, "�"))
	}
	lc := map[string]string{}
	for k, v := range cur.Vars {
		if k != localeLangVar {
			lc[strings.ToValidUTF8(k, "�")] = strings.ToValidUTF8(v, "�")
		}
	}
	// An empty map in the configuration stays empty rather than null, so
	// that it does not show up as a change.
	if len(lc) > 0 || (!state.LC.IsNull() && !state.LC.IsUnknown()) {
		m, d := types.MapValueFrom(ctx, types.StringType, lc)
		resp.Diagnostics.Append(d...)
		state.LC = m
	} else {
		state.LC = types.MapNull(types.StringType)
	}
	if state.Generate.IsNull() {
		state.Generate = types.BoolValue(false)
	}
	if state.RestoreOnDestroy.IsNull() {
		state.RestoreOnDestroy = types.BoolValue(false)
	}
	state.ID = types.StringValue(singletonID)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *localeResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state localeModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() || !state.RestoreOnDestroy.ValueBool() {
		return
	}
	raw, diags := req.Private.GetKey(ctx, localePrivatePrevious)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	if len(raw) == 0 {
		resp.Diagnostics.AddWarning("Locale not restored",
			"The locale that was set before this resource was created is not known, for example because the resource was imported. The current locale was left in place.")
		return
	}
	var prev localeFileState
	if err := json.Unmarshal(raw, &prev); err != nil {
		resp.Diagnostics.AddError("Invalid private state", fmt.Sprintf("Decoding the recorded locale: %s.", err))
		return
	}
	// State and private state are not validated by the schema; never write
	// arbitrary values to a file that shells source.
	for k, v := range prev.Vars {
		if !isManagedLocaleVar(k) || !localeVarPattern.MatchString(k) || validateLocaleName(v) != nil {
			resp.Diagnostics.AddError("Invalid private state", fmt.Sprintf("The recorded locale holds an invalid assignment %s=%q.", k, v))
			return
		}
	}
	if state.Path.IsNull() || state.Path.ValueString() == "" {
		resp.Diagnostics.AddError("Invalid state", "The state holds no path.")
		return
	}
	_, host, diags := r.resolvePath(&state)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	if err := writeLocaleVars(host, prev.Vars, !prev.Exists); err != nil {
		resp.Diagnostics.AddError("Restoring locale", capitalize(err.Error())+".")
	}
}

// ImportState accepts only the ID "system". The locale file is detected as
// if path were not set.
func (r *localeResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	if req.ID != singletonID {
		resp.Diagnostics.AddError("Invalid import ID", fmt.Sprintf("The import ID of sysutils_locale must be %q, got %q.", singletonID, req.ID))
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), singletonID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("generate"), false)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("restore_on_destroy"), false)...)
}
