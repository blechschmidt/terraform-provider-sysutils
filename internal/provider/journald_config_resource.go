package provider

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework-validators/mapvalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/attr"
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
	_ resource.Resource                   = (*journaldConfigResource)(nil)
	_ resource.ResourceWithConfigure      = (*journaldConfigResource)(nil)
	_ resource.ResourceWithImportState    = (*journaldConfigResource)(nil)
	_ resource.ResourceWithModifyPlan     = (*journaldConfigResource)(nil)
	_ resource.ResourceWithValidateConfig = (*journaldConfigResource)(nil)
)

func NewJournaldConfigResource() resource.Resource { return &journaldConfigResource{} }

type journaldConfigResource struct {
	cfg *journaldConfig
	// service detects systemd and runs systemctl for restart.
	service *serviceConfig
	// root is the provider's root_dir; the drop-in is inside it.
	root *fsRoot
}

type journaldConfigModel struct {
	Name            types.String `tfsdk:"name"`
	Storage         types.String `tfsdk:"storage"`
	SystemMaxUse    types.String `tfsdk:"system_max_use"`
	MaxRetentionSec types.String `tfsdk:"max_retention_sec"`
	Compress        types.Bool   `tfsdk:"compress"`
	ForwardToSyslog types.Bool   `tfsdk:"forward_to_syslog"`
	Extra           types.Map    `tfsdk:"extra"`
	Restart         types.Bool   `tfsdk:"restart"`
	Content         types.String `tfsdk:"content"`
	Path            types.String `tfsdk:"path"`
	Mode            types.String `tfsdk:"mode"`
	Owner           types.String `tfsdk:"owner"`
	Group           types.String `tfsdk:"group"`
	ID              types.String `tfsdk:"id"`
}

func (r *journaldConfigResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_journald_config"
}

func (r *journaldConfigResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages a systemd-journald drop-in, `/etc/systemd/journald.conf.d/<name>.conf`, below the provider's `root_dir` if that is set, with settings of the `[Journal]` section of `journald.conf(5)`. " +
			"The file is written atomically with mode `0644`, owned by the user running Terraform (root), under the provider's shared-file lock, and is removed on destroy. " +
			"Settings that are not set are not written, so the defaults and other drop-ins apply to them; drop-ins are read in the order of their names, and a later one wins. " +
			"journald reads its configuration only when it starts: set `restart` to restart it when the file changes. Requires root privileges, unless used with `root_dir` on a tree you can write.",
		Attributes: map[string]schema.Attribute{
			"name": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "Name of the drop-in without the `.conf` suffix, such as `\"90-hardening\"`. " +
					"Must consist of letters, digits and `_.+@-`, and must not start with `.`, `+` or `-`. Changing this forces a new resource.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
				Validators:    []validator.String{stringCheck("journald drop-in name", validateJournaldName)},
			},
			"storage": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "`Storage=`: where to store the journal. `\"persistent\"` keeps it on disk in `/var/log/journal`, `\"volatile\"` in memory only, " +
					"`\"auto\"` on disk if `/var/log/journal` exists, and `\"none\"` drops all logs (forwarding still works).",
				Validators: []validator.String{stringvalidator.OneOf(journaldStorageValues...)},
			},
			"system_max_use": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "`SystemMaxUse=`: how much disk space the persistent journal may use, as a size with an optional suffix K, M, G, T, P or E (base 1024), such as `\"500M\"`.",
				Validators:          []validator.String{stringCheck("size", validateJournaldSize)},
			},
			"max_retention_sec": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "`MaxRetentionSec=`: the longest time to keep journal entries, as a systemd time span such as `\"1month\"`, `\"2weeks\"` or `\"36h\"`; `\"0\"` turns time-based removal off.",
				Validators:          []validator.String{stringCheck("time span", validateJournaldTimespan)},
			},
			"compress": schema.BoolAttribute{
				Optional:            true,
				MarkdownDescription: "`Compress=`: whether to compress large journal entries. Use `extra` for a size threshold instead of a boolean.",
			},
			"forward_to_syslog": schema.BoolAttribute{
				Optional:            true,
				MarkdownDescription: "`ForwardToSyslog=`: whether to forward log messages to a traditional syslog daemon.",
			},
			"extra": schema.MapAttribute{
				ElementType: types.StringType,
				Optional:    true,
				MarkdownDescription: "Other settings of the `[Journal]` section, by name, such as `{ RateLimitBurst = \"10000\", MaxFileSec = \"1week\" }`. They are written after the typed settings, sorted by name. " +
					"Names must be capitalised setting names; values must be single lines without leading or trailing white space and must not end with a backslash. " +
					"A key of a typed attribute that is set, such as `Storage` with `storage`, is an error; unset, the key can hold a value the typed attribute can't, such as `Compress = \"64K\"`. " +
					"journald ignores names it does not know, with a warning in its log.",
				Validators: []validator.Map{
					mapvalidator.KeysAre(stringCheck("journald setting name", validateJournaldKey)),
					mapvalidator.ValueStringsAre(stringCheck("journald setting value", validateJournaldValue)),
				},
			},
			"restart": schema.BoolAttribute{
				Optional: true,
				Computed: true,
				Default:  booldefault.StaticBool(false),
				MarkdownDescription: "Whether to run `systemctl restart systemd-journald.service` after the file was created, changed or removed, so that journald applies it. " +
					"Needs systemd to run the host, which is checked when planning, and cannot be used with `root_dir`. " +
					"If the restart fails, apply fails, and the next apply writes the file and restarts journald again. Defaults to `false`.",
			},
			"content": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "The rendered file. Refresh reads the file, so any change made outside Terraform shows up as a planned change to this attribute, " +
					"which apply reverts.",
			},
			"path": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Path of the file, `/etc/systemd/journald.conf.d/<name>.conf`.",
			},
			"mode": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Mode of the file; always `\"0644\"` after apply. A different mode on disk shows up as drift.",
			},
			"owner": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Owner of the file; the user running Terraform (`\"root\"`) after apply. A different owner on disk shows up as drift.",
			},
			"group": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Group of the file; the primary group of the user running Terraform after apply. A different group on disk shows up as drift.",
			},
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Resource identifier (equal to `name`).",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
		},
	}
}

func (r *journaldConfigResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	data, diags := providerDataFrom(req.ProviderData)
	resp.Diagnostics.Append(diags...)
	if data != nil {
		r.cfg = data.journald
		r.service = data.service
		r.root = data.root
	}
}

// fsRoot returns the configured root, or the host root before
// configuration.
func (r *journaldConfigResource) fsRoot() *fsRoot {
	if r.root == nil {
		return hostRoot
	}
	return r.root
}

// spec converts m to a journaldSpec. known is false if any part of it is
// unknown.
func (m *journaldConfigModel) spec(ctx context.Context) (s *journaldSpec, known bool, diags diag.Diagnostics) {
	for _, v := range []types.String{m.Storage, m.SystemMaxUse, m.MaxRetentionSec} {
		if v.IsUnknown() {
			return nil, false, diags
		}
	}
	if m.Compress.IsUnknown() || m.ForwardToSyslog.IsUnknown() || m.Extra.IsUnknown() {
		return nil, false, diags
	}
	s = &journaldSpec{
		storage:         m.Storage.ValueString(),
		systemMaxUse:    m.SystemMaxUse.ValueString(),
		maxRetentionSec: m.MaxRetentionSec.ValueString(),
		compress:        m.Compress.ValueBoolPointer(),
		forwardToSyslog: m.ForwardToSyslog.ValueBoolPointer(),
		extra:           map[string]string{},
	}
	var extra map[string]types.String
	diags.Append(m.Extra.ElementsAs(ctx, &extra, false)...)
	if diags.HasError() {
		return nil, false, diags
	}
	for k, v := range extra {
		if v.IsUnknown() {
			return nil, false, diags
		}
		s.extra[k] = v.ValueString()
	}
	return s, true, diags
}

// setSpec sets the typed attributes of m from s, as parsed on import.
func (m *journaldConfigModel) setSpec(s *journaldSpec) {
	optString := func(v string) types.String {
		if v == "" {
			return types.StringNull()
		}
		return types.StringValue(v)
	}
	m.Storage = optString(s.storage)
	m.SystemMaxUse = optString(s.systemMaxUse)
	m.MaxRetentionSec = optString(s.maxRetentionSec)
	m.Compress = types.BoolPointerValue(s.compress)
	m.ForwardToSyslog = types.BoolPointerValue(s.forwardToSyslog)
	m.Extra = types.MapNull(types.StringType)
	if len(s.extra) > 0 {
		elems := make(map[string]attr.Value, len(s.extra))
		for k, v := range s.extra {
			elems[k] = types.StringValue(v)
		}
		m.Extra = types.MapValueMust(types.StringType, elems)
	}
}

// ValidateConfig checks the keys of extra against the typed attributes.
func (r *journaldConfigResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var cfg journaldConfigModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}
	s, known, diags := cfg.spec(ctx)
	resp.Diagnostics.Append(diags...)
	if !known || resp.Diagnostics.HasError() {
		return
	}
	if a, err := s.validateCombination(); err != nil {
		resp.Diagnostics.AddAttributeError(path.Root(a), "Invalid journald configuration", capitalize(err.Error())+".")
	}
}

// ModifyPlan sets the computed attributes to what apply writes, so that
// refresh reading anything else from the file plans an update, and refuses
// restart where journald can't be restarted.
func (r *journaldConfigResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.Plan.Raw.IsNull() {
		return // Destroy.
	}
	var plan journaldConfigModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if plan.Restart.ValueBool() {
		if err := r.checkRestart(); err != nil {
			resp.Diagnostics.AddAttributeError(path.Root("restart"), "Cannot restart journald", capitalize(err.Error())+".")
			return
		}
	}
	if plan.Name.IsUnknown() {
		plan.Path, plan.ID = types.StringUnknown(), types.StringUnknown()
	} else {
		plan.Path = types.StringValue(r.cfg.filePath(plan.Name.ValueString()))
		plan.ID = plan.Name
	}
	plan.Content = types.StringUnknown()
	s, known, diags := plan.spec(ctx)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	pending := false
	if !req.State.Raw.IsNull() && plan.Restart.ValueBool() {
		pending, diags = journaldRestartPending(ctx, req.Private)
		resp.Diagnostics.Append(diags...)
	}
	switch {
	case pending:
		// The last restart failed: plan an update to retry it.
		resp.Diagnostics.AddAttributeWarning(path.Root("restart"), "journald restart pending",
			"Restarting journald failed after the drop-in was last updated; this apply restarts it again.")
	case known:
		if _, err := s.validate(); err == nil {
			plan.Content = types.StringValue(s.render())
		}
	}
	uid, gid := processOwner()
	plan.Mode = types.StringValue(formatMode(journaldFileMode))
	plan.Owner = types.StringValue(uidName(uid))
	plan.Group = types.StringValue(gidName(gid))
	resp.Diagnostics.Append(resp.Plan.Set(ctx, &plan)...)
}

// checkRestart reports why journald can't be restarted: root_dir is set,
// or systemd does not run the host.
func (r *journaldConfigResource) checkRestart() error {
	if !r.fsRoot().isHost() {
		return fmt.Errorf("restarting journald would restart the journald of the running host, not of the tree below root_dir %s; set restart = false with root_dir", r.fsRoot())
	}
	kind, err := r.service.detect()
	if err != nil {
		return fmt.Errorf("restarting journald needs systemd: %w", err)
	}
	if kind != initSystemSystemd {
		return fmt.Errorf("restarting journald needs systemd, but %s manages services on this host", kind)
	}
	return nil
}

// restartJournald restarts journald after checking again that it can be.
func (r *journaldConfigResource) restartJournald(ctx context.Context) error {
	if err := r.checkRestart(); err != nil {
		return err
	}
	sc := systemctl{run: r.service.runner(), timeout: mustParseDuration(defaultServiceTimeout)}
	return sc.do(ctx, "restart", "--", journaldUnit)
}

func (r *journaldConfigResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan journaldConfigModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(r.write(ctx, &plan, true)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
	if plan.Restart.ValueBool() {
		// The file is in place, so the resource is kept in state; the error
		// taints it, and the next apply replaces it and restarts again.
		if err := r.restartJournald(ctx); err != nil {
			resp.Diagnostics.AddAttributeError(path.Root("restart"), "Restarting journald", fmt.Sprintf("%s was written, but restarting journald failed: %s.", plan.Path.ValueString(), err))
		}
	}
}

func (r *journaldConfigResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, prior journaldConfigModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &prior)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(r.write(ctx, &plan, false)...)
	if resp.Diagnostics.HasError() {
		return
	}
	pending, diags := journaldRestartPending(ctx, req.Private)
	resp.Diagnostics.Append(diags...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if !plan.Restart.ValueBool() || (plan.Content.Equal(prior.Content) && !pending) {
		// Without restart, a pending restart is dropped with it.
		resp.Diagnostics.Append(resp.Private.SetKey(ctx, journaldPrivateRestartPending, nil)...)
		return
	}
	if err := r.restartJournald(ctx); err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("restart"), "Restarting journald", fmt.Sprintf("%s was written, but restarting journald failed: %s.", plan.Path.ValueString(), err))
		// The file is up to date, so remember that the restart is still
		// due; ModifyPlan plans an update for it.
		resp.Diagnostics.Append(resp.Private.SetKey(ctx, journaldPrivateRestartPending, []byte("true"))...)
		return
	}
	resp.Diagnostics.Append(resp.Private.SetKey(ctx, journaldPrivateRestartPending, nil)...)
}

// journaldPrivateRestartPending is the private state key that records a
// restart that failed after the file was updated.
const journaldPrivateRestartPending = "journald_restart_pending"

// journaldRestartPending reports whether a restart failed after the last
// update of the file.
func journaldRestartPending(ctx context.Context, private privateGetter) (bool, diag.Diagnostics) {
	raw, diags := private.GetKey(ctx, journaldPrivateRestartPending)
	return string(raw) == "true", diags
}

// write renders plan, writes it to the drop-in and sets the computed
// attributes of plan. With create set, an existing file is an error.
func (r *journaldConfigResource) write(ctx context.Context, plan *journaldConfigModel, create bool) (diags diag.Diagnostics) {
	name := plan.Name.ValueString()
	if err := validateJournaldName(name); err != nil {
		diags.AddAttributeError(path.Root("name"), "Invalid name", capitalize(err.Error())+".")
		return diags
	}
	s, known, d := plan.spec(ctx)
	diags.Append(d...)
	if diags.HasError() {
		return diags
	}
	if !known {
		diags.AddError("Unknown configuration", "All attributes of sysutils_journald_config must be known at apply time.")
		return diags
	}
	if a, err := s.validate(); err != nil {
		diags.AddAttributeError(path.Root(a), "Invalid journald configuration", capitalize(err.Error())+".")
		return diags
	}
	content := s.render()
	p := r.cfg.filePath(name)
	host, err := r.fsRoot().resolve(p)
	if err != nil {
		diags.AddAttributeError(path.Root("name"), "Unable to resolve path", capitalize(err.Error())+".")
		return diags
	}
	uid, gid := processOwner()
	spec := dropInSpec{mode: journaldFileMode, dirMode: 0o755, maxSize: maxJournaldFileSize, uid: uid, gid: gid}
	if err := spec.write(host, []byte(content), create, nil); err != nil {
		if errors.Is(err, errDropInExists) {
			diags.AddAttributeError(path.Root("name"), "journald drop-in already exists",
				fmt.Sprintf("%s already exists. Import it with \"terraform import\" and the ID %q instead, or remove the file.", p, name))
		} else {
			diags.AddError("Writing journald drop-in", fmt.Sprintf("Writing %s: %s.", p, err))
		}
		return diags
	}
	plan.ID = types.StringValue(name)
	plan.Path = types.StringValue(p)
	plan.Content = types.StringValue(content)
	plan.Mode = types.StringValue(formatMode(journaldFileMode))
	plan.Owner = types.StringValue(uidName(uid))
	plan.Group = types.StringValue(gidName(gid))
	return diags
}

func (r *journaldConfigResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state journaldConfigModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	name := state.Name.ValueString()
	// State is not validated by the schema; never read an arbitrary file.
	if err := validateJournaldName(name); err != nil {
		resp.Diagnostics.AddError("Invalid state", capitalize(err.Error())+".")
		return
	}
	imported := state.Content.IsNull()
	p := r.cfg.filePath(name)
	host, err := r.fsRoot().resolve(p)
	if err != nil {
		resp.Diagnostics.AddError("Unable to resolve path", capitalize(err.Error())+".")
		return
	}
	data, snap, err := dropInSpec{maxSize: maxJournaldFileSize}.read(host)
	if err != nil {
		resp.Diagnostics.AddError("Reading journald drop-in", fmt.Sprintf("Reading %s: %s.", p, err))
		return
	}
	if snap == nil {
		if imported {
			resp.Diagnostics.AddError("Cannot import journald drop-in", fmt.Sprintf("%s does not exist.", p))
			return
		}
		resp.State.RemoveResource(ctx)
		return
	}
	content := strings.ToValidUTF8(string(data), "�")
	if imported {
		s, err := parseJournaldFile(content)
		if err != nil {
			resp.Diagnostics.AddError("Cannot import journald drop-in", fmt.Sprintf("%s: %s.", p, capitalize(err.Error())))
			return
		}
		state.setSpec(s)
	}
	if state.Restart.IsNull() {
		state.Restart = types.BoolValue(false)
	}
	state.ID = types.StringValue(name)
	state.Path = types.StringValue(p)
	state.Content = types.StringValue(content)
	state.Mode = types.StringValue(formatMode(snap.mode))
	state.Owner = types.StringValue(uidName(snap.uid))
	state.Group = types.StringValue(gidName(snap.gid))
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *journaldConfigResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state journaldConfigModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	name := state.Name.ValueString()
	// State is not validated by the schema; never remove an arbitrary file.
	if err := validateJournaldName(name); err != nil {
		resp.Diagnostics.AddError("Invalid state", capitalize(err.Error())+".")
		return
	}
	p := r.cfg.filePath(name)
	host, err := r.fsRoot().resolve(p)
	if err != nil {
		resp.Diagnostics.AddError("Unable to resolve path", capitalize(err.Error())+".")
		return
	}
	if _, err := removeDropInFile(host); err != nil {
		resp.Diagnostics.AddError("Removing journald drop-in", fmt.Sprintf("Removing %s: %s.", p, err))
		return
	}
	if !state.Restart.ValueBool() {
		return
	}
	// Also when the file was already gone, so that a restart that failed
	// on an earlier attempt to destroy is retried.
	if !r.fsRoot().isHost() {
		resp.Diagnostics.AddWarning("journald not restarted",
			fmt.Sprintf("%s was removed below root_dir %s; the journald of the running host was not restarted.", p, r.fsRoot()))
		return
	}
	if err := r.restartJournald(ctx); err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("restart"), "Restarting journald", fmt.Sprintf("%s was removed, but restarting journald failed: %s.", p, err))
	}
}

// ImportState imports a drop-in by its name without the ".conf" suffix.
// Read parses the file into the attributes.
func (r *journaldConfigResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	if err := validateJournaldName(req.ID); err != nil {
		resp.Diagnostics.AddError("Invalid import ID",
			fmt.Sprintf("Import ID must be the name of a drop-in in the journald.conf.d directory without the \".conf\" suffix: %s.", capitalize(err.Error())))
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("name"), req.ID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), req.ID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("restart"), false)...)
}
