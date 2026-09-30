package provider

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework-validators/listvalidator"
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
	_ resource.Resource                   = (*logrotateResource)(nil)
	_ resource.ResourceWithConfigure      = (*logrotateResource)(nil)
	_ resource.ResourceWithImportState    = (*logrotateResource)(nil)
	_ resource.ResourceWithModifyPlan     = (*logrotateResource)(nil)
	_ resource.ResourceWithValidateConfig = (*logrotateResource)(nil)
)

func NewLogrotateResource() resource.Resource { return &logrotateResource{} }

type logrotateResource struct {
	cfg *logrotateConfig
	// root is the provider's root_dir; the drop-in is inside it.
	root *fsRoot
}

type logrotateModel struct {
	Name            types.String `tfsdk:"name"`
	Paths           types.List   `tfsdk:"paths"`
	Frequency       types.String `tfsdk:"frequency"`
	Rotate          types.Int64  `tfsdk:"rotate"`
	Compress        types.Bool   `tfsdk:"compress"`
	Delaycompress   types.Bool   `tfsdk:"delaycompress"`
	Missingok       types.Bool   `tfsdk:"missingok"`
	Notifempty      types.Bool   `tfsdk:"notifempty"`
	Sharedscripts   types.Bool   `tfsdk:"sharedscripts"`
	CreateMode      types.String `tfsdk:"create_mode"`
	CreateOwner     types.String `tfsdk:"create_owner"`
	CreateGroup     types.String `tfsdk:"create_group"`
	Postrotate      types.String `tfsdk:"postrotate"`
	ExtraDirectives types.List   `tfsdk:"extra_directives"`
	Validate        types.Bool   `tfsdk:"validate"`
	Content         types.String `tfsdk:"content"`
	Path            types.String `tfsdk:"path"`
	Mode            types.String `tfsdk:"mode"`
	Owner           types.String `tfsdk:"owner"`
	Group           types.String `tfsdk:"group"`
	ID              types.String `tfsdk:"id"`
}

func (r *logrotateResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_logrotate"
}

// logrotateFlag returns the schema of a directive that is on, off or, if
// unset, inherited from the global configuration.
func logrotateFlag(on, off, what string) schema.BoolAttribute {
	return schema.BoolAttribute{
		Optional: true,
		MarkdownDescription: fmt.Sprintf("`true` writes `%s` (%s), `false` writes `%s`. If unset, neither is written and the global setting of `/etc/logrotate.conf` applies.",
			on, what, off),
	}
}

func (r *logrotateResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages a logrotate drop-in file in `/etc/logrotate.d`, below the provider's `root_dir` if that is set, holding one block that rotates the log files in `paths`. " +
			"The file is written atomically with mode `0644`, owned by the user running Terraform (root), under the provider's shared-file lock, and is checked with `logrotate -d` before it is renamed into place when logrotate is installed. " +
			"It is removed on destroy. Requires root privileges, unless used with `root_dir` on a tree you can write.",
		Attributes: map[string]schema.Attribute{
			"name": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "Name of the file in `/etc/logrotate.d`, such as `\"myapp\"`. " +
					"Must consist of letters, digits and `_.+@-`, must not start with `.`, `+`, `@` or `-`, and must not end with an extension logrotate skips, such as `.bak`, `.disabled`, `.dpkg-old`, `.rpmsave` or `~`. " +
					"Changing this forces a new resource.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
				Validators:    []validator.String{stringCheck("logrotate drop-in name", validateLogrotateName)},
			},
			"paths": schema.ListAttribute{
				ElementType: types.StringType,
				Required:    true,
				MarkdownDescription: "The log files to rotate, as absolute paths that may contain the glob characters `*`, `?` and `[...]`, such as `\"/var/log/myapp/*.log\"`. " +
					"Paths with spaces are written in double quotes. Paths must not contain quotes, backslashes, `{`, `}`, `#`, `//`, `.` or `..` components. " +
					"With `root_dir`, they are written as given: they name the files of the system that boots from the tree.",
				Validators: []validator.List{
					listvalidator.SizeAtLeast(1),
					listvalidator.UniqueValues(),
					listvalidator.ValueStringsAre(stringCheck("log path", validateLogrotatePath)),
				},
			},
			"frequency": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "How often to rotate: `\"hourly\"`, `\"daily\"`, `\"weekly\"`, `\"monthly\"` or `\"yearly\"`. `hourly` needs logrotate to run hourly, which it does not by default. If unset, the global setting applies.",
				Validators:          []validator.String{stringvalidator.OneOf(logrotateFrequencies...)},
			},
			"rotate": schema.Int64Attribute{
				Optional:            true,
				MarkdownDescription: "How many rotated files to keep (`rotate <count>`). `0` removes old versions instead of keeping any, `-1` never removes them. If unset, the global setting applies.",
				Validators:          []validator.Int64{int64validator.Between(-1, maxLogrotateRotate)},
			},
			"compress":      logrotateFlag("compress", "nocompress", "compress rotated files with gzip"),
			"delaycompress": logrotateFlag("delaycompress", "nodelaycompress", "compress each file only at the rotation after the one that created it, for programs that keep writing to it for a while"),
			"missingok":     logrotateFlag("missingok", "nomissingok", "a missing log file is not an error"),
			"notifempty":    logrotateFlag("notifempty", "ifempty", "empty log files are not rotated"),
			"sharedscripts": logrotateFlag("sharedscripts", "nosharedscripts", "run `postrotate` once for all matching files rather than once per file"),
			"create_mode": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "Writes `create <mode> <owner> <group>`, so that an empty log file with this mode, such as `\"0640\"`, is created right after rotation. " +
					"`create_owner` and `create_group` are optional, but must be set together: logrotate 3.22 and later read `create <mode> <owner>` as an owner and a group. If unset, no `create` directive is written; use `extra_directives` for `nocreate` or a bare `create`.",
				Validators: []validator.String{octalMode()},
			},
			"create_owner": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Owner of the log file created after rotation. Requires `create_mode` and `create_group`.",
				Validators:          []validator.String{accountName()},
			},
			"create_group": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Group of the log file created after rotation. Requires `create_owner`.",
				Validators:          []validator.String{accountName()},
			},
			"postrotate": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "Shell script to run after rotation, written between `postrotate` and `endscript`, such as `\"systemctl kill -s HUP myapp.service\"`. " +
					"It is written verbatim, followed by a newline if it does not end with one, and must not contain a line that starts with `endscript` after blanks, such as `endscript; true`: logrotate ends the script there.",
				Validators: []validator.String{stringCheck("postrotate script", validateLogrotateScript)},
			},
			"extra_directives": schema.ListAttribute{
				ElementType: types.StringType,
				Optional:    true,
				MarkdownDescription: "Further directives, one line each, written verbatim after the typed ones, such as `\"maxsize 100M\"`, `\"dateext\"` or `\"su root adm\"`. " +
					"A directive must not conflict with a typed attribute that is set (such as `\"nocompress\"` with `compress`), must not be a comment, and must not start a script or contain `{` or `}`; " +
					"scripts other than `postrotate` are not supported.",
				Validators: []validator.List{
					listvalidator.ValueStringsAre(stringCheck("logrotate directive", validateLogrotateDirective)),
				},
			},
			"validate": schema.BoolAttribute{
				Optional: true,
				Computed: true,
				Default:  booldefault.StaticBool(true),
				MarkdownDescription: "Whether to check the new file with `logrotate -d` before it is installed, if logrotate is installed on the host; without logrotate the file is installed unchecked. " +
					"If logrotate reports an error in the file, apply fails with its output and the file on disk is left as it was. " +
					"Errors about the log files themselves, such as missing ones, are not errors in the file and are ignored. " +
					"Below the provider's `root_dir` the check is skipped: logrotate would look up the users, groups and log files of the host rather than of the tree. " +
					"Defaults to `true`.",
			},
			"content": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "The rendered file. Refresh reads the file, so any change made outside Terraform shows up as a planned change to this attribute, " +
					"which apply reverts.",
			},
			"path": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Path of the file, `/etc/logrotate.d/<name>`.",
			},
			"mode": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Mode of the file; always `\"0644\"` after apply. A different mode on disk shows up as drift: logrotate ignores files writable by group or others.",
			},
			"owner": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Owner of the file; the user running Terraform (`\"root\"`) after apply. A different owner on disk shows up as drift: logrotate ignores files owned by other users.",
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

func (r *logrotateResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	data, diags := providerDataFrom(req.ProviderData)
	resp.Diagnostics.Append(diags...)
	if data != nil {
		r.cfg = data.logrotate
		r.root = data.root
	}
}

// fsRoot returns the configured root, or the host root before
// configuration.
func (r *logrotateResource) fsRoot() *fsRoot {
	if r.root == nil {
		return hostRoot
	}
	return r.root
}

// spec converts m to a logrotateSpec. known is false if any part of it is
// unknown.
func (m *logrotateModel) spec(ctx context.Context) (s *logrotateSpec, known bool, diags diag.Diagnostics) {
	s = &logrotateSpec{}
	strs := func(l types.List) ([]string, bool) {
		if l.IsUnknown() {
			return nil, false
		}
		var elems []types.String
		diags.Append(l.ElementsAs(ctx, &elems, false)...)
		out := make([]string, 0, len(elems))
		for _, e := range elems {
			if e.IsUnknown() {
				return nil, false
			}
			out = append(out, e.ValueString())
		}
		return out, true
	}
	var ok bool
	if s.paths, ok = strs(m.Paths); !ok || diags.HasError() {
		return nil, false, diags
	}
	if s.extra, ok = strs(m.ExtraDirectives); !ok || diags.HasError() {
		return nil, false, diags
	}
	for _, v := range []types.String{m.Frequency, m.CreateMode, m.CreateOwner, m.CreateGroup, m.Postrotate} {
		if v.IsUnknown() {
			return nil, false, diags
		}
	}
	for _, v := range []types.Bool{m.Compress, m.Delaycompress, m.Missingok, m.Notifempty, m.Sharedscripts} {
		if v.IsUnknown() {
			return nil, false, diags
		}
	}
	if m.Rotate.IsUnknown() {
		return nil, false, diags
	}
	s.frequency = m.Frequency.ValueString()
	s.rotate = m.Rotate.ValueInt64Pointer()
	s.compress = m.Compress.ValueBoolPointer()
	s.delaycompress = m.Delaycompress.ValueBoolPointer()
	s.missingok = m.Missingok.ValueBoolPointer()
	s.notifempty = m.Notifempty.ValueBoolPointer()
	s.sharedscripts = m.Sharedscripts.ValueBoolPointer()
	s.createMode = m.CreateMode.ValueString()
	s.createOwner = m.CreateOwner.ValueString()
	s.createGroup = m.CreateGroup.ValueString()
	s.postrotate = m.Postrotate.ValueString()
	return s, true, diags
}

// setSpec sets the typed attributes of m from s, as parsed on import.
func (m *logrotateModel) setSpec(s *logrotateSpec) {
	optString := func(v string) types.String {
		if v == "" {
			return types.StringNull()
		}
		return types.StringValue(v)
	}
	list := func(v []string) types.List {
		elems := make([]attr.Value, len(v))
		for i, e := range v {
			elems[i] = types.StringValue(e)
		}
		return types.ListValueMust(types.StringType, elems)
	}
	m.Paths = list(s.paths)
	m.ExtraDirectives = types.ListNull(types.StringType)
	if len(s.extra) > 0 {
		m.ExtraDirectives = list(s.extra)
	}
	m.Frequency = optString(s.frequency)
	m.Rotate = types.Int64PointerValue(s.rotate)
	m.Compress = types.BoolPointerValue(s.compress)
	m.Delaycompress = types.BoolPointerValue(s.delaycompress)
	m.Missingok = types.BoolPointerValue(s.missingok)
	m.Notifempty = types.BoolPointerValue(s.notifempty)
	m.Sharedscripts = types.BoolPointerValue(s.sharedscripts)
	m.CreateMode = optString(s.createMode)
	m.CreateOwner = optString(s.createOwner)
	m.CreateGroup = optString(s.createGroup)
	m.Postrotate = optString(s.postrotate)
}

// ValidateConfig checks the attributes against each other: the parts of
// create, and extra directives against the typed attributes.
func (r *logrotateResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var cfg logrotateModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}
	s, known, diags := cfg.spec(ctx)
	resp.Diagnostics.Append(diags...)
	if !known || resp.Diagnostics.HasError() {
		return
	}
	// The attributes' own validators check each of them.
	if a, err := s.validateCombination(); err != nil {
		resp.Diagnostics.AddAttributeError(path.Root(a), "Invalid logrotate configuration", capitalize(err.Error())+".")
	}
}

// ModifyPlan sets the computed attributes to what apply writes, so that
// refresh reading anything else from the file (other contents, mode or
// ownership) plans an update.
func (r *logrotateResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.Plan.Raw.IsNull() {
		return // Destroy.
	}
	var plan logrotateModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
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
	if known {
		if _, err := s.validate(); err == nil {
			plan.Content = types.StringValue(s.render())
		}
	}
	uid, gid := processOwner()
	plan.Mode = types.StringValue(formatMode(logrotateFileMode))
	plan.Owner = types.StringValue(uidName(uid))
	plan.Group = types.StringValue(gidName(gid))
	resp.Diagnostics.Append(resp.Plan.Set(ctx, &plan)...)
}

func (r *logrotateResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan logrotateModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(r.write(ctx, &plan, true)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *logrotateResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan logrotateModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(r.write(ctx, &plan, false)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// write renders plan, checks it with logrotate if plan asks for that and
// logrotate is installed, writes it to the drop-in and sets the computed
// attributes of plan. With create set, an existing file is an error.
func (r *logrotateResource) write(ctx context.Context, plan *logrotateModel, create bool) (diags diag.Diagnostics) {
	name := plan.Name.ValueString()
	if err := validateLogrotateName(name); err != nil {
		diags.AddAttributeError(path.Root("name"), "Invalid name", capitalize(err.Error())+".")
		return diags
	}
	s, known, d := plan.spec(ctx)
	diags.Append(d...)
	if diags.HasError() {
		return diags
	}
	if !known {
		diags.AddError("Unknown configuration", "All attributes of sysutils_logrotate must be known at apply time.")
		return diags
	}
	if a, err := s.validate(); err != nil {
		diags.AddAttributeError(path.Root(a), "Invalid logrotate configuration", capitalize(err.Error())+".")
		return diags
	}
	content := s.render()

	p := r.cfg.filePath(name)
	host, err := r.fsRoot().resolve(p)
	if err != nil {
		diags.AddAttributeError(path.Root("name"), "Unable to resolve path", capitalize(err.Error())+".")
		return diags
	}

	var check func(tmp string) error
	if (plan.Validate.IsNull() || plan.Validate.ValueBool()) && r.fsRoot().isHost() {
		if bin := r.cfg.logrotate(); bin != "" {
			run := r.cfg.runner()
			check = func(tmp string) error { return checkLogrotateFile(ctx, run, bin, tmp, p) }
		}
	}

	uid, gid := processOwner()
	// logrotate reads hidden files in logrotate.d too, but never those
	// ending in "~", so a run at the wrong moment can't see the temporary
	// file as a second block for the same logs.
	spec := dropInSpec{mode: logrotateFileMode, dirMode: 0o755, maxSize: maxLogrotateFileSize, uid: uid, gid: gid, tmpSuffix: "~"}
	if err := spec.write(host, []byte(content), create, check); err != nil {
		var lerr *logrotateError
		switch {
		case errors.As(err, &lerr):
			diags.AddError("Invalid logrotate file",
				fmt.Sprintf("logrotate -d rejected the new contents of %s, so the file was not changed:\n\n%s", p, lerr.output))
		case errors.Is(err, errDropInExists):
			diags.AddAttributeError(path.Root("name"), "logrotate drop-in already exists",
				fmt.Sprintf("%s already exists. Import it with \"terraform import\" and the ID %q instead, or remove the file.", p, name))
		default:
			diags.AddError("Writing logrotate drop-in", fmt.Sprintf("Writing %s: %s.", p, err))
		}
		return diags
	}
	plan.ID = types.StringValue(name)
	plan.Path = types.StringValue(p)
	plan.Content = types.StringValue(content)
	plan.Mode = types.StringValue(formatMode(logrotateFileMode))
	plan.Owner = types.StringValue(uidName(uid))
	plan.Group = types.StringValue(gidName(gid))
	return diags
}

func (r *logrotateResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state logrotateModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	name := state.Name.ValueString()
	// State is not validated by the schema; never read an arbitrary file.
	if err := validateLogrotateName(name); err != nil {
		resp.Diagnostics.AddError("Invalid state", capitalize(err.Error())+".")
		return
	}
	imported := state.Content.IsNull() && state.Paths.IsNull()
	p := r.cfg.filePath(name)
	host, err := r.fsRoot().resolve(p)
	if err != nil {
		resp.Diagnostics.AddError("Unable to resolve path", capitalize(err.Error())+".")
		return
	}
	data, snap, err := dropInSpec{maxSize: maxLogrotateFileSize}.read(host)
	if err != nil {
		resp.Diagnostics.AddError("Reading logrotate drop-in", fmt.Sprintf("Reading %s: %s.", p, err))
		return
	}
	if snap == nil {
		if imported {
			resp.Diagnostics.AddError("Cannot import logrotate drop-in", fmt.Sprintf("%s does not exist.", p))
			return
		}
		resp.State.RemoveResource(ctx)
		return
	}
	content := strings.ToValidUTF8(string(data), "�")
	if imported {
		s, err := parseLogrotateFile(content)
		if err != nil {
			resp.Diagnostics.AddError("Cannot import logrotate drop-in", fmt.Sprintf("%s: %s.", p, capitalize(err.Error())))
			return
		}
		state.setSpec(s)
		state.Validate = types.BoolValue(true)
	}
	state.ID = types.StringValue(name)
	state.Path = types.StringValue(p)
	state.Content = types.StringValue(content)
	state.Mode = types.StringValue(formatMode(snap.mode))
	state.Owner = types.StringValue(uidName(snap.uid))
	state.Group = types.StringValue(gidName(snap.gid))
	if state.Validate.IsNull() {
		state.Validate = types.BoolValue(true)
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *logrotateResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state logrotateModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	name := state.Name.ValueString()
	// State is not validated by the schema; never remove an arbitrary file.
	if err := validateLogrotateName(name); err != nil {
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
		resp.Diagnostics.AddError("Removing logrotate drop-in", fmt.Sprintf("Removing %s: %s.", p, err))
	}
}

// ImportState imports a drop-in by the name of its file in logrotate.d.
// Read parses the file into the attributes.
func (r *logrotateResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	if err := validateLogrotateName(req.ID); err != nil {
		resp.Diagnostics.AddError("Invalid import ID",
			fmt.Sprintf("Import ID must be the name of a file in the logrotate.d directory: %s.", capitalize(err.Error())))
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("name"), req.ID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), req.ID)...)
}
