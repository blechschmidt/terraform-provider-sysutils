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
	_ resource.Resource                = (*timezoneResource)(nil)
	_ resource.ResourceWithConfigure   = (*timezoneResource)(nil)
	_ resource.ResourceWithImportState = (*timezoneResource)(nil)
)

// timezonePrivatePrevious is the private state key of the configuration
// found before the first apply, as a JSON timezoneSnapshot.
const timezonePrivatePrevious = "previous"

func NewTimezoneResource() resource.Resource { return &timezoneResource{} }

type timezoneResource struct {
	rootedResource
	cfg *timezoneConfig
}

type timezoneModel struct {
	Timezone         types.String `tfsdk:"timezone"`
	RestoreOnDestroy types.Bool   `tfsdk:"restore_on_destroy"`
	ID               types.String `tfsdk:"id"`
}

func (r *timezoneResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_timezone"
}

func (r *timezoneResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Sets the system time zone: `/etc/localtime` becomes a symlink to the zone's file in `/usr/share/zoneinfo`, and `/etc/timezone` names the zone on distributions that use it (Debian, Ubuntu, Alpine). " +
			"Uses `timedatectl` when systemd is PID 1 and the provider's `root_dir` is not set. " +
			"There is one system time zone, so there must be at most one `sysutils_timezone` per host (or per `root_dir`). " +
			"On destroy the zone stays as it is, unless `restore_on_destroy` is set.",
		Attributes: map[string]schema.Attribute{
			"timezone": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "IANA time zone name, such as `\"Europe/Berlin\"`, `\"America/New_York\"` or `\"UTC\"`. " +
					"Components separated by `/` of letters, digits, `_`, `+` and `-`. " +
					"When applying, `/usr/share/zoneinfo/<timezone>` must be a compiled zone file (inside the provider's `root_dir` if set), so tzdata must be installed.",
				Validators: []validator.String{stringCheck("time zone", validateTimezoneName)},
			},
			"restore_on_destroy": schema.BoolAttribute{
				Optional: true,
				Computed: true,
				Default:  booldefault.StaticBool(false),
				MarkdownDescription: "Whether destroy puts back the `/etc/localtime` and `/etc/timezone` that were there before the resource was created. " +
					"They are recorded in the resource's private state on create; after import there is nothing to restore, and destroy only warns. " +
					"Defaults to `false`: destroy leaves the time zone as it is.",
			},
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Always `\"" + singletonID + "\"`.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
		},
	}
}

func (r *timezoneResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	data, diags := providerDataFrom(req.ProviderData)
	resp.Diagnostics.Append(diags...)
	if data != nil {
		r.cfg = data.timezone
		r.fsRoot = data.root
	}
}

func (r *timezoneResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan timezoneModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	// Record what is there now, before anything is changed, so that
	// destroy can put it back.
	prev, err := readTimezoneSnapshot(r.root())
	if err != nil {
		resp.Diagnostics.AddError("Reading time zone", capitalize(err.Error())+".")
		return
	}
	raw, err := json.Marshal(prev)
	if err != nil {
		resp.Diagnostics.AddError("Encoding private state", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.Private.SetKey(ctx, timezonePrivatePrevious, raw)...)
	if resp.Diagnostics.HasError() {
		return
	}

	resp.Diagnostics.Append(r.apply(ctx, plan.Timezone.ValueString())...)
	if resp.Diagnostics.HasError() {
		return
	}
	plan.ID = types.StringValue(singletonID)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *timezoneResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan timezoneModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(r.apply(ctx, plan.Timezone.ValueString())...)
	if resp.Diagnostics.HasError() {
		return
	}
	plan.ID = types.StringValue(singletonID)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// apply sets the zone name.
func (r *timezoneResource) apply(ctx context.Context, name string) diag.Diagnostics {
	var diags diag.Diagnostics
	// Config validation already enforces this; re-check as defense in depth.
	if err := validateTimezoneName(name); err != nil {
		diags.AddAttributeError(path.Root("timezone"), "Invalid time zone", capitalize(err.Error())+".")
		return diags
	}
	warnings, err := setTimezone(ctx, r.cfg, r.root(), name)
	for _, w := range warnings {
		diags.AddWarning("timedatectl failed", w)
	}
	if err != nil {
		diags.AddAttributeError(path.Root("timezone"), "Setting time zone", capitalize(err.Error())+".")
	}
	return diags
}

func (r *timezoneResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state timezoneModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	snap, err := readTimezoneSnapshot(r.root())
	if err != nil {
		resp.Diagnostics.AddError("Reading time zone", capitalize(err.Error())+".")
		return
	}
	imported := state.Timezone.IsNull()
	zone, isZone := snap.zone(r.root(), state.Timezone.ValueString())
	switch {
	case imported && !isZone:
		resp.Diagnostics.AddError("Cannot import time zone",
			fmt.Sprintf("%s does not select a time zone in %s: it is %s.", localtimePath, zoneinfoDir, describeLocaltime(snap.Localtime)))
		return
	case zone == "":
		// Nothing selects a zone; the plan shows it being set again.
		state.Timezone = types.StringNull()
	default:
		// Not necessarily a zone name: a foreign symlink target or the
		// content of /etc/timezone, which need not be valid UTF-8.
		state.Timezone = types.StringValue(strings.ToValidUTF8(zone, "�"))
	}
	if state.RestoreOnDestroy.IsNull() {
		state.RestoreOnDestroy = types.BoolValue(false)
	}
	state.ID = types.StringValue(singletonID)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// describeLocaltime describes e for error messages.
func describeLocaltime(e localtimeEntry) string {
	switch e.Kind {
	case localtimeAbsent:
		return "missing"
	case localtimeSymlink:
		return fmt.Sprintf("a symlink to %q", e.Target)
	default:
		return "a regular file that is not a copy of the zone named in " + timezoneFilePath
	}
}

func (r *timezoneResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state timezoneModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() || !state.RestoreOnDestroy.ValueBool() {
		return
	}
	raw, diags := req.Private.GetKey(ctx, timezonePrivatePrevious)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	if len(raw) == 0 {
		resp.Diagnostics.AddWarning("Time zone not restored",
			"The time zone that was set before this resource was created is not known, for example because the resource was imported. The current time zone was left in place.")
		return
	}
	var prev timezoneSnapshot
	if err := json.Unmarshal(raw, &prev); err != nil {
		resp.Diagnostics.AddError("Invalid private state", fmt.Sprintf("Decoding the recorded time zone: %s.", err))
		return
	}
	warnings, err := restoreTimezone(ctx, r.cfg, r.root(), &prev)
	for _, w := range warnings {
		resp.Diagnostics.AddWarning("timedatectl failed", w)
	}
	if err != nil {
		resp.Diagnostics.AddError("Restoring time zone", capitalize(err.Error())+".")
	}
}

// ImportState accepts only the ID "system".
func (r *timezoneResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	if req.ID != singletonID {
		resp.Diagnostics.AddError("Invalid import ID", fmt.Sprintf("The import ID of sysutils_timezone must be %q, got %q.", singletonID, req.ID))
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), singletonID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("restore_on_destroy"), false)...)
}
