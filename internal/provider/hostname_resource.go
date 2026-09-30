package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

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
	_ resource.Resource                = (*hostnameResource)(nil)
	_ resource.ResourceWithConfigure   = (*hostnameResource)(nil)
	_ resource.ResourceWithImportState = (*hostnameResource)(nil)
	_ resource.ResourceWithModifyPlan  = (*hostnameResource)(nil)
)

const (
	// hostnamePrivatePrevious is the private state key of the
	// configuration found before the first apply, as a JSON
	// hostnameSnapshot.
	hostnamePrivatePrevious = "previous"
	// hostnamePrivateApplied is the private state key of the static
	// hostname last applied, as a JSON string. The /etc/hosts line was
	// written for it, even if the state holds another name after drift.
	hostnamePrivateApplied = "applied"
)

func NewHostnameResource() resource.Resource { return &hostnameResource{} }

type hostnameResource struct {
	rootedResource
	cfg *hostnameConfig
}

type hostnameModel struct {
	Hostname          types.String `tfsdk:"hostname"`
	PrettyHostname    types.String `tfsdk:"pretty_hostname"`
	ManageHostsEntry  types.Bool   `tfsdk:"manage_hosts_entry"`
	RestoreOnDestroy  types.Bool   `tfsdk:"restore_on_destroy"`
	TransientHostname types.String `tfsdk:"transient_hostname"`
	ID                types.String `tfsdk:"id"`
}

func (r *hostnameResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_hostname"
}

func (r *hostnameResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Sets the host's static hostname in `/etc/hostname` and its kernel (transient) hostname, and optionally the pretty hostname in `/etc/machine-info` and a `" + hostnameLoopbackIP + "` line in `/etc/hosts`. " +
			"Uses `hostnamectl` when systemd is PID 1 and the provider's `root_dir` is not set. Below `root_dir` only the files are written. " +
			"There is one hostname per host, so there must be at most one `sysutils_hostname` per host (or per `root_dir`). " +
			"On destroy the hostname stays as it is, unless `restore_on_destroy` is set.",
		Attributes: map[string]schema.Attribute{
			"hostname": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "Static hostname, such as `\"web1\"` or `\"web1.example.com\"`, as defined by RFC 1123: labels of 1 to 63 letters, digits and hyphens that neither start nor end with a hyphen, separated by dots, at most 253 characters in all and without a trailing dot. " +
					"The kernel holds at most 64 characters, so without `root_dir` the name must not be longer. " +
					"Written to `/etc/hostname` and, without `root_dir`, set as the kernel hostname.",
				Validators: []validator.String{stringCheck("hostname", validateHostname)},
			},
			"pretty_hostname": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "Free-form pretty hostname, such as `\"Web server 1\"`, stored as `PRETTY_HOSTNAME` in `/etc/machine-info` and shown by `hostnamectl`. " +
					"Up to 255 bytes of UTF-8 without control characters or leading or trailing white space. " +
					"Unset leaves the pretty hostname alone.",
				Validators: []validator.String{stringCheck("pretty hostname", validatePrettyHostname)},
			},
			"manage_hosts_entry": schema.BoolAttribute{
				Optional: true,
				Computed: true,
				Default:  booldefault.StaticBool(false),
				MarkdownDescription: "Keep a line in `/etc/hosts` that maps `" + hostnameLoopbackIP + "` to `hostname` and, for a name with dots, its first label (`" + hostnameLoopbackIP + " web1.example.com web1`), as Debian's installer writes it, so that the hostname resolves without DNS. " +
					"A `" + hostnameLoopbackIP + "` line for the previous hostname is updated in place; other lines are kept. " +
					"Turning it off leaves the line in place. Defaults to `false`.",
			},
			"restore_on_destroy": schema.BoolAttribute{
				Optional: true,
				Computed: true,
				Default:  booldefault.StaticBool(false),
				MarkdownDescription: "Whether destroy puts back the hostnames that were set before the resource was created: `/etc/hostname`, the kernel hostname, the pretty hostname if `pretty_hostname` is set, and the `/etc/hosts` line if `manage_hosts_entry` is set. " +
					"They are recorded in the resource's private state on create; after import there is nothing to restore, and destroy only warns. " +
					"Defaults to `false`: destroy leaves the hostname as it is.",
			},
			"transient_hostname": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "The kernel hostname (`uname -n`). Equal to `hostname` after apply; a plan shows a change of this attribute when the kernel hostname was changed outside Terraform, for example by DHCP. " +
					"Null when the provider's `root_dir` is set, since the kernel hostname is not managed then.",
			},
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Always `\"" + singletonID + "\"`.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
		},
	}
}

func (r *hostnameResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	data, diags := providerDataFrom(req.ProviderData)
	resp.Diagnostics.Append(diags...)
	if data != nil {
		r.cfg = data.hostname
		r.fsRoot = data.root
	}
}

// ModifyPlan plans the kernel hostname: equal to hostname on the host root,
// so that a kernel hostname changed outside Terraform shows up as a change,
// and null below root_dir. A name the kernel cannot hold is refused here
// rather than halfway through apply.
func (r *hostnameResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.Plan.Raw.IsNull() {
		return // Destroy.
	}
	var plan hostnameModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	transient := types.StringNull()
	if r.cfg.managesKernel(r.root()) {
		transient = plan.Hostname
		if !plan.Hostname.IsUnknown() && !plan.Hostname.IsNull() {
			if err := validateKernelHostname(plan.Hostname.ValueString()); err != nil {
				resp.Diagnostics.AddAttributeError(path.Root("hostname"), "Hostname too long for the kernel", capitalize(err.Error())+".")
				return
			}
		}
	}
	resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("transient_hostname"), transient)...)
}

func (r *hostnameResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan hostnameModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	// Record what is there now, before anything is changed, so that
	// destroy can put it back.
	prev, err := readHostnameSnapshot(r.cfg, r.root())
	if err != nil {
		resp.Diagnostics.AddError("Reading hostname", capitalize(err.Error())+".")
		return
	}
	raw, err := json.Marshal(prev)
	if err != nil {
		resp.Diagnostics.AddError("Encoding private state", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.Private.SetKey(ctx, hostnamePrivatePrevious, raw)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(r.apply(ctx, &plan, prev.static())...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(setPrivateJSON(ctx, resp.Private, hostnamePrivateApplied, plan.Hostname.ValueString())...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *hostnameResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state hostnameModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	applied, diags := getPrivateString(ctx, req.Private, hostnamePrivateApplied)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(r.apply(ctx, &plan, applied, state.Hostname.ValueString())...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(setPrivateJSON(ctx, resp.Private, hostnamePrivateApplied, plan.Hostname.ValueString())...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// apply sets the hostnames in plan. previous are hostnames whose /etc/hosts
// line may be taken over.
func (r *hostnameResource) apply(ctx context.Context, plan *hostnameModel, previous ...string) diag.Diagnostics {
	var diags diag.Diagnostics
	spec := hostnameSpec{
		Static:        plan.Hostname.ValueString(),
		HostsLine:     plan.ManageHostsEntry.ValueBool(),
		PreviousNames: previous,
	}
	if !plan.PrettyHostname.IsNull() {
		pretty := plan.PrettyHostname.ValueString()
		spec.Pretty = &pretty
	}
	warnings, err := setHostname(ctx, r.cfg, r.root(), spec)
	for _, w := range warnings {
		diags.AddWarning(w.Summary, w.Detail)
	}
	if err != nil {
		diags.AddAttributeError(path.Root("hostname"), "Setting hostname", capitalize(err.Error())+".")
		return diags
	}
	plan.TransientHostname = types.StringNull()
	if r.cfg.managesKernel(r.root()) {
		plan.TransientHostname = plan.Hostname
	}
	plan.ID = types.StringValue(singletonID)
	return diags
}

func (r *hostnameResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state hostnameModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	snap, err := readHostnameSnapshot(r.cfg, r.root())
	if err != nil {
		resp.Diagnostics.AddError("Reading hostname", capitalize(err.Error())+".")
		return
	}
	imported := state.Hostname.IsNull()
	static := snap.static()
	if imported && static == "" {
		resp.Diagnostics.AddError("Cannot import hostname",
			fmt.Sprintf("%s is missing or holds no hostname, so there is no static hostname to import.", hostnameFilePath))
		return
	}

	// The /etc/hosts line was written for the hostname last applied.
	applied, diags := getPrivateString(ctx, req.Private, hostnamePrivateApplied)
	resp.Diagnostics.Append(diags...)
	if applied == "" {
		applied = state.Hostname.ValueString()
	}

	state.Hostname = types.StringNull()
	if static != "" {
		// Not necessarily a valid hostname, nor valid UTF-8.
		state.Hostname = types.StringValue(strings.ToValidUTF8(static, "�"))
	}
	if !state.PrettyHostname.IsNull() {
		state.PrettyHostname = types.StringNull()
		if snap.Pretty != nil {
			state.PrettyHostname = types.StringValue(strings.ToValidUTF8(*snap.Pretty, "�"))
		}
	}
	state.TransientHostname = types.StringNull()
	if r.cfg.managesKernel(r.root()) {
		state.TransientHostname = types.StringValue(strings.ToValidUTF8(snap.Kernel, "�"))
	}
	if state.ManageHostsEntry.IsNull() {
		state.ManageHostsEntry = types.BoolValue(false)
	}
	if state.ManageHostsEntry.ValueBool() {
		// A missing or changed line shows up as manage_hosts_entry
		// changing from false to true, and is written again on apply.
		p, err := hostnamePathsIn(r.root())
		if err != nil {
			resp.Diagnostics.AddError("Reading hostname", capitalize(err.Error())+".")
			return
		}
		current, err := hostnameHostsLineCurrent(p.hosts, applied)
		if err != nil {
			resp.Diagnostics.AddError("Reading hosts file", capitalize(err.Error())+".")
			return
		}
		state.ManageHostsEntry = types.BoolValue(current)
	}
	if state.RestoreOnDestroy.IsNull() {
		state.RestoreOnDestroy = types.BoolValue(false)
	}
	state.ID = types.StringValue(singletonID)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *hostnameResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state hostnameModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() || !state.RestoreOnDestroy.ValueBool() {
		return
	}
	raw, diags := req.Private.GetKey(ctx, hostnamePrivatePrevious)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	if len(raw) == 0 {
		resp.Diagnostics.AddWarning("Hostname not restored",
			"The hostname that was set before this resource was created is not known, for example because the resource was imported. The current hostname was left in place.")
		return
	}
	var prev hostnameSnapshot
	if err := json.Unmarshal(raw, &prev); err != nil {
		resp.Diagnostics.AddError("Invalid private state", fmt.Sprintf("Decoding the recorded hostname: %s.", err))
		return
	}
	applied, diags := getPrivateString(ctx, req.Private, hostnamePrivateApplied)
	resp.Diagnostics.Append(diags...)
	if applied == "" {
		applied = state.Hostname.ValueString()
	}
	warnings, err := restoreHostname(ctx, r.cfg, r.root(), &prev, applied, !state.PrettyHostname.IsNull(), state.ManageHostsEntry.ValueBool())
	for _, w := range warnings {
		resp.Diagnostics.AddWarning(w.Summary, w.Detail)
	}
	if err != nil {
		resp.Diagnostics.AddError("Restoring hostname", capitalize(err.Error())+".")
	}
}

// ImportState accepts only the ID "system".
func (r *hostnameResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	if req.ID != singletonID {
		resp.Diagnostics.AddError("Invalid import ID", fmt.Sprintf("The import ID of sysutils_hostname must be %q, got %q.", singletonID, req.ID))
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), singletonID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("manage_hosts_entry"), false)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("restore_on_destroy"), false)...)
}

// privateState is implemented by the private state of requests and
// responses.
type privateState interface {
	GetKey(ctx context.Context, key string) ([]byte, diag.Diagnostics)
}

type privateStateWriter interface {
	SetKey(ctx context.Context, key string, value []byte) diag.Diagnostics
}

// getPrivateString returns the JSON string stored under key, or "".
func getPrivateString(ctx context.Context, p privateState, key string) (string, diag.Diagnostics) {
	raw, diags := p.GetKey(ctx, key)
	if diags.HasError() || len(raw) == 0 {
		return "", diags
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		diags.AddError("Invalid private state", fmt.Sprintf("Decoding %q: %s.", key, err))
	}
	return s, diags
}

// setPrivateJSON stores v as JSON under key.
func setPrivateJSON(ctx context.Context, p privateStateWriter, key string, v any) diag.Diagnostics {
	raw, err := json.Marshal(v)
	if err != nil {
		var diags diag.Diagnostics
		diags.AddError("Encoding private state", err.Error())
		return diags
	}
	return p.SetKey(ctx, key, raw)
}
