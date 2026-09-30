package provider

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework-validators/listvalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/resourcevalidator"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/listdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var (
	_ resource.Resource                     = (*sudoersResource)(nil)
	_ resource.ResourceWithConfigure        = (*sudoersResource)(nil)
	_ resource.ResourceWithImportState      = (*sudoersResource)(nil)
	_ resource.ResourceWithModifyPlan       = (*sudoersResource)(nil)
	_ resource.ResourceWithConfigValidators = (*sudoersResource)(nil)
)

func NewSudoersResource() resource.Resource { return &sudoersResource{} }

type sudoersResource struct {
	cfg *sudoersConfig
	// root is the provider's root_dir; the drop-in is inside it.
	root *fsRoot
}

type sudoersModel struct {
	Name     types.String `tfsdk:"name"`
	Content  types.String `tfsdk:"content"`
	Rules    types.List   `tfsdk:"rules"`
	Validate types.Bool   `tfsdk:"validate"`
	Path     types.String `tfsdk:"path"`
	Mode     types.String `tfsdk:"mode"`
	Owner    types.String `tfsdk:"owner"`
	Group    types.String `tfsdk:"group"`
	ID       types.String `tfsdk:"id"`
}

type sudoersRuleModel struct {
	Users    types.List   `tfsdk:"users"`
	Hosts    types.List   `tfsdk:"hosts"`
	Runas    types.String `tfsdk:"runas"`
	Commands types.List   `tfsdk:"commands"`
	Nopasswd types.Bool   `tfsdk:"nopasswd"`
	Setenv   types.Bool   `tfsdk:"setenv"`
}

func (r *sudoersResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_sudoers"
}

func (r *sudoersResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages a sudo drop-in file in `/etc/sudoers.d`, below the provider's `root_dir` if that is set. " +
			"The file is written atomically with mode `0440` and owned by `root:root`, and is checked with `visudo -cf` before it is renamed into place, so that a syntax error never locks everyone out of sudo. " +
			"It is removed on destroy. Requires root privileges.",
		Attributes: map[string]schema.Attribute{
			"name": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "Name of the file in `/etc/sudoers.d`, such as `\"90-deploy\"`. " +
					"Must not contain `.` or `/`, white space or control characters, and must not end with `~`: sudo silently skips files in `sudoers.d` whose names contain a `.` or end with `~`. " +
					"Changing this forces a new resource.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
				Validators:    []validator.String{stringCheck("sudoers drop-in name", validateSudoersName)},
			},
			"content": schema.StringAttribute{
				Optional: true,
				Computed: true,
				MarkdownDescription: "Contents of the file, in sudoers syntax, written verbatim. Exactly one of `content` and `rules` must be set; with `rules`, this is the rendered file. " +
					"Refresh reads the file, so any change made outside Terraform shows up as a planned change to this attribute.",
				Validators: []validator.String{stringCheck("sudoers content", validateSudoersContent)},
			},
			"rules": schema.ListNestedAttribute{
				Optional: true,
				MarkdownDescription: "User specifications to write instead of `content`, one line each, after a line saying that the file is managed by Terraform: " +
					"`<users> <hosts> = (<runas>) NOPASSWD: SETENV: <commands>`. Exactly one of `content` and `rules` must be set.",
				Validators: []validator.List{listvalidator.SizeAtLeast(1)},
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"users": schema.ListAttribute{
							ElementType: types.StringType,
							Required:    true,
							MarkdownDescription: "Who the rule applies to: user names, `%group`, `#uid`, `%#gid`, `+netgroup`, user aliases or `ALL`, each prefixed with `!` to exclude it. " +
								"Elements must not contain white space, `,`, `=`, `:` or parentheses.",
							Validators: []validator.List{
								listvalidator.SizeAtLeast(1),
								listvalidator.ValueStringsAre(stringCheck("sudoers user", validateSudoersToken("user"))),
							},
						},
						"hosts": schema.ListAttribute{
							ElementType: types.StringType,
							Optional:    true,
							Computed:    true,
							Default:     listdefault.StaticValue(types.ListValueMust(types.StringType, []attr.Value{types.StringValue("ALL")})),
							MarkdownDescription: "Hosts the rule applies on: host names, addresses, networks, `+netgroup`, host aliases or `ALL`. " +
								"Defaults to `[\"ALL\"]`.",
							Validators: []validator.List{
								listvalidator.SizeAtLeast(1),
								listvalidator.ValueStringsAre(stringCheck("sudoers host", validateSudoersToken("host"))),
							},
						},
						"runas": schema.StringAttribute{
							Optional: true,
							MarkdownDescription: "Who the commands may be run as, written in parentheses: `\"ALL\"`, `\"ALL:ALL\"`, `\"postgres\"` or `\":docker\"` (users, then groups after a `:`). " +
								"If unset, the commands may only be run as root.",
							Validators: []validator.String{stringCheck("sudoers runas", validateSudoersRunas)},
						},
						"commands": schema.ListAttribute{
							ElementType: types.StringType,
							Required:    true,
							MarkdownDescription: "Commands the users may run, such as `\"/usr/bin/systemctl restart app\"`, `\"sudoedit /etc/app.conf\"` or `\"ALL\"`. " +
								"Each element must be a single command: `,`, `:` and `=` in arguments must be escaped with a backslash (`\"\\\\,\"` in HCL), as sudoers requires.",
							Validators: []validator.List{
								listvalidator.SizeAtLeast(1),
								listvalidator.ValueStringsAre(stringCheck("sudoers command", validateSudoersCommand)),
							},
						},
						"nopasswd": schema.BoolAttribute{
							Optional:            true,
							Computed:            true,
							Default:             booldefault.StaticBool(false),
							MarkdownDescription: "Whether the commands may be run without a password (`NOPASSWD:`). Defaults to `false`.",
						},
						"setenv": schema.BoolAttribute{
							Optional:            true,
							Computed:            true,
							Default:             booldefault.StaticBool(false),
							MarkdownDescription: "Whether the user may keep environment variables that sudo would otherwise remove (`SETENV:`). Defaults to `false`.",
						},
					},
				},
			},
			"validate": schema.BoolAttribute{
				Optional: true,
				Computed: true,
				Default:  booldefault.StaticBool(true),
				MarkdownDescription: "Whether to check the new file with `visudo -cf` before it is installed. If visudo rejects it, apply fails with visudo's output and the file on disk is left as it was. " +
					"On the host, visudo must then be installed. Below the provider's `root_dir`, the check is skipped with a warning if the tree has no `visudo`, or if the host running Terraform has none to run: programs in the tree are never run. " +
					"Defaults to `true`.",
			},
			"path": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Path of the file, `/etc/sudoers.d/<name>`.",
			},
			"mode": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Mode of the file; always `\"0440\"` after apply. A different mode on disk shows up as drift.",
			},
			"owner": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Owner of the file; always `\"root\"` after apply. A different owner on disk shows up as drift.",
			},
			"group": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Group of the file; always the group with GID 0 (`\"root\"`) after apply. A different group on disk shows up as drift.",
			},
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Resource identifier (equal to `name`).",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
		},
	}
}

func (r *sudoersResource) ConfigValidators(context.Context) []resource.ConfigValidator {
	return []resource.ConfigValidator{
		resourcevalidator.ExactlyOneOf(path.MatchRoot("content"), path.MatchRoot("rules")),
	}
}

func (r *sudoersResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	data, diags := providerDataFrom(req.ProviderData)
	resp.Diagnostics.Append(diags...)
	if data != nil {
		r.cfg = data.sudoers
		r.root = data.root
	}
}

// fsRoot returns the configured root, or the host root before
// configuration.
func (r *sudoersResource) fsRoot() *fsRoot {
	if r.root == nil {
		return hostRoot
	}
	return r.root
}

// hostPath returns the host path of the drop-in name, which must be valid,
// resolved inside root_dir like the paths of the file resources, so that
// neither root_dir nor a symlink in the tree can make the resource write the
// sudoers of the host.
func (r *sudoersResource) hostPath(name string) (string, error) {
	return r.fsRoot().resolve(r.cfg.filePath(name))
}

// ModifyPlan sets the computed attributes to what apply writes, so that
// refresh reading anything else from the file (other contents, mode or
// ownership) plans an update.
func (r *sudoersResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.Plan.Raw.IsNull() {
		return // Destroy.
	}
	var plan sudoersModel
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
	if !plan.Rules.IsNull() {
		plan.Content = types.StringUnknown()
		if content, known, diags := sudoersRulesContent(ctx, plan.Rules); diags.HasError() {
			resp.Diagnostics.Append(diags...)
			return
		} else if known {
			plan.Content = types.StringValue(content)
		}
	}
	uid, gid := r.cfg.owner()
	plan.Mode = types.StringValue(formatMode(sudoersFileMode))
	plan.Owner = types.StringValue(uidName(uid))
	plan.Group = types.StringValue(gidName(gid))
	resp.Diagnostics.Append(resp.Plan.Set(ctx, &plan)...)
}

// sudoersRulesContent renders the drop-in for rules. known is false if any
// part of them is unknown.
func sudoersRulesContent(ctx context.Context, rules types.List) (content string, known bool, diags diag.Diagnostics) {
	if rules.IsUnknown() {
		return "", false, diags
	}
	var models []sudoersRuleModel
	diags.Append(rules.ElementsAs(ctx, &models, false)...)
	if diags.HasError() {
		return "", false, diags
	}
	parsed := make([]sudoersRule, 0, len(models))
	for i, m := range models {
		if m.Runas.IsUnknown() || m.Nopasswd.IsUnknown() || m.Setenv.IsUnknown() {
			return "", false, diags
		}
		rule := sudoersRule{runas: m.Runas.ValueString(), nopasswd: m.Nopasswd.ValueBool(), setenv: m.Setenv.ValueBool()}
		for _, l := range []struct {
			v   types.List
			dst *[]string
		}{{m.Users, &rule.users}, {m.Hosts, &rule.hosts}, {m.Commands, &rule.commands}} {
			if l.v.IsUnknown() {
				return "", false, diags
			}
			var elems []types.String
			diags.Append(l.v.ElementsAs(ctx, &elems, false)...)
			if diags.HasError() {
				return "", false, diags
			}
			for _, e := range elems {
				if e.IsUnknown() {
					return "", false, diags
				}
				*l.dst = append(*l.dst, e.ValueString())
			}
		}
		if err := rule.validate(); err != nil {
			diags.AddAttributeError(path.Root("rules").AtListIndex(i), "Invalid sudoers rule", capitalize(err.Error())+".")
			return "", false, diags
		}
		parsed = append(parsed, rule)
	}
	return renderSudoersRules(parsed), true, diags
}

func (r *sudoersResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan sudoersModel
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

func (r *sudoersResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan sudoersModel
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

// write renders plan, checks it with visudo if plan asks for that, and
// writes it to the drop-in, and sets the computed attributes of plan. With
// create set, an existing file is an error.
func (r *sudoersResource) write(ctx context.Context, plan *sudoersModel, create bool) (diags diag.Diagnostics) {
	name := plan.Name.ValueString()
	if err := validateSudoersName(name); err != nil {
		diags.AddAttributeError(path.Root("name"), "Invalid name", capitalize(err.Error())+".")
		return diags
	}
	var content string
	if plan.Rules.IsNull() {
		if plan.Content.IsUnknown() || plan.Content.IsNull() {
			diags.AddAttributeError(path.Root("content"), "Unknown content", "content must be known at apply time.")
			return diags
		}
		content = plan.Content.ValueString()
	} else {
		var known bool
		var d diag.Diagnostics
		content, known, d = sudoersRulesContent(ctx, plan.Rules)
		diags.Append(d...)
		if diags.HasError() {
			return diags
		}
		if !known {
			diags.AddAttributeError(path.Root("rules"), "Unknown rules", "All attributes of the rules must be known at apply time.")
			return diags
		}
	}
	if err := validateSudoersContent(content); err != nil {
		diags.AddAttributeError(path.Root("content"), "Invalid sudoers content", capitalize(err.Error())+".")
		return diags
	}

	p := r.cfg.filePath(name)
	host, err := r.hostPath(name)
	if err != nil {
		diags.AddAttributeError(path.Root("name"), "Unable to resolve path", capitalize(err.Error())+".")
		return diags
	}

	var check func(tmp string) error
	if plan.Validate.IsNull() || plan.Validate.ValueBool() {
		bin, skipped, err := r.cfg.visudo(r.fsRoot())
		switch {
		case errors.Is(err, errVisudoMissing):
			diags.AddAttributeError(path.Root("validate"), "visudo not found",
				fmt.Sprintf("%s is checked with visudo before it is installed, but %s. Install sudo first, or set validate = false to install the file unchecked.", p, err))
			return diags
		case err != nil:
			diags.AddError("Finding visudo", capitalize(err.Error())+".")
			return diags
		case skipped != "":
			diags.AddAttributeWarning(path.Root("validate"), "sudoers file not validated",
				fmt.Sprintf("%s was installed without checking it with visudo: %s. Set validate = false to silence this warning.", p, skipped))
		default:
			run := r.cfg.runner()
			check = func(tmp string) error { return checkSudoersFile(ctx, run, bin, tmp, p) }
		}
	}

	uid, gid := r.cfg.owner()
	if err := writeSudoersFile(host, []byte(content), uid, gid, create, check); err != nil {
		var verr *visudoError
		switch {
		case errors.As(err, &verr):
			diags.AddError("Invalid sudoers file",
				fmt.Sprintf("visudo -cf rejected the new contents of %s, so the file was not changed:\n\n%s", p, verr.output))
		case errors.Is(err, errSudoersFileExists):
			diags.AddAttributeError(path.Root("name"), "Sudoers drop-in already exists",
				fmt.Sprintf("%s already exists. Import it with \"terraform import\" instead, or remove the file.", p))
		default:
			diags.AddError("Writing sudoers drop-in", fmt.Sprintf("Writing %s: %s.", p, err))
		}
		return diags
	}
	plan.ID = types.StringValue(name)
	plan.Path = types.StringValue(p)
	plan.Content = types.StringValue(content)
	plan.Mode = types.StringValue(formatMode(sudoersFileMode))
	plan.Owner = types.StringValue(uidName(uid))
	plan.Group = types.StringValue(gidName(gid))
	return diags
}

func (r *sudoersResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state sudoersModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	name := state.Name.ValueString()
	// State is not validated by the schema; never read an arbitrary file.
	if err := validateSudoersName(name); err != nil {
		resp.Diagnostics.AddError("Invalid state", capitalize(err.Error())+".")
		return
	}
	imported := state.Content.IsNull() && state.Rules.IsNull()
	p := r.cfg.filePath(name)
	host, err := r.hostPath(name)
	if err != nil {
		resp.Diagnostics.AddError("Unable to resolve path", capitalize(err.Error())+".")
		return
	}
	data, snap, err := readSudoersFile(host)
	if err != nil {
		resp.Diagnostics.AddError("Reading sudoers drop-in", fmt.Sprintf("Reading %s: %s.", p, err))
		return
	}
	if snap == nil {
		if imported {
			resp.Diagnostics.AddError("Cannot import sudoers drop-in", fmt.Sprintf("%s does not exist.", p))
			return
		}
		resp.State.RemoveResource(ctx)
		return
	}
	state.ID = types.StringValue(name)
	state.Path = types.StringValue(p)
	state.Content = types.StringValue(strings.ToValidUTF8(string(data), "�"))
	state.Mode = types.StringValue(formatMode(snap.mode))
	state.Owner = types.StringValue(uidName(snap.uid))
	state.Group = types.StringValue(gidName(snap.gid))
	if state.Validate.IsNull() {
		state.Validate = types.BoolValue(true)
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *sudoersResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state sudoersModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	name := state.Name.ValueString()
	// State is not validated by the schema; never remove an arbitrary file.
	if err := validateSudoersName(name); err != nil {
		resp.Diagnostics.AddError("Invalid state", capitalize(err.Error())+".")
		return
	}
	p := r.cfg.filePath(name)
	host, err := r.hostPath(name)
	if err != nil {
		resp.Diagnostics.AddError("Unable to resolve path", capitalize(err.Error())+".")
		return
	}
	if err := removeSudoersFile(host); err != nil {
		resp.Diagnostics.AddError("Removing sudoers drop-in", fmt.Sprintf("Removing %s: %s.", p, err))
	}
}

// ImportState imports a drop-in by the name of its file in sudoers.d.
func (r *sudoersResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	if err := validateSudoersName(req.ID); err != nil {
		resp.Diagnostics.AddError("Invalid import ID",
			fmt.Sprintf("Import ID must be the name of a file in the sudoers.d directory: %s.", capitalize(err.Error())))
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("name"), req.ID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), req.ID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("validate"), true)...)
}
