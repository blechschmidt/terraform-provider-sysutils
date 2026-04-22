package provider

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"os/user"
	"strconv"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var (
	_ resource.Resource                = (*userResource)(nil)
	_ resource.ResourceWithImportState = (*userResource)(nil)
)

func NewUserResource() resource.Resource { return &userResource{} }

type userResource struct{}

type userModel struct {
	Name       types.String `tfsdk:"name"`
	UID        types.Int64  `tfsdk:"uid"`
	GID        types.Int64  `tfsdk:"gid"`
	Home       types.String `tfsdk:"home"`
	Shell      types.String `tfsdk:"shell"`
	Comment    types.String `tfsdk:"comment"`
	System     types.Bool   `tfsdk:"system"`
	CreateHome types.Bool   `tfsdk:"create_home"`
	Groups     types.Set    `tfsdk:"groups"`
	ID         types.String `tfsdk:"id"`
}

func (r *userResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_user"
}

func (r *userResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Manages a local Linux user via useradd/usermod/userdel. Requires root privileges.",
		Attributes: map[string]schema.Attribute{
			"name": schema.StringAttribute{
				Required:      true,
				Description:   "Username.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"uid": schema.Int64Attribute{
				Optional:    true,
				Computed:    true,
				Description: "Numeric user ID. Assigned by the system if unset.",
			},
			"gid": schema.Int64Attribute{
				Optional:    true,
				Computed:    true,
				Description: "Primary numeric group ID.",
			},
			"home": schema.StringAttribute{
				Optional:    true,
				Computed:    true,
				Description: "Home directory path.",
			},
			"shell": schema.StringAttribute{
				Optional:    true,
				Computed:    true,
				Description: "Login shell.",
			},
			"comment": schema.StringAttribute{
				Optional:    true,
				Description: "GECOS comment field.",
			},
			"system": schema.BoolAttribute{
				Optional:      true,
				Description:   "Create as a system account.",
				PlanModifiers: []planmodifier.Bool{},
			},
			"create_home": schema.BoolAttribute{
				Optional:    true,
				Description: "Create the home directory on create. Defaults to false.",
			},
			"groups": schema.SetAttribute{
				Optional:    true,
				ElementType: types.StringType,
				Description: "Supplementary group names.",
			},
			"id": schema.StringAttribute{
				Computed:      true,
				Description:   "Resource identifier (the username).",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
		},
	}
}

func runCmd(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func groupsFromSet(ctx context.Context, s types.Set) ([]string, error) {
	if s.IsNull() || s.IsUnknown() {
		return nil, nil
	}
	var out []string
	diags := s.ElementsAs(ctx, &out, false)
	if diags.HasError() {
		return nil, errors.New(diags.Errors()[0].Detail())
	}
	return out, nil
}

func (r *userResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan userModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	name := plan.Name.ValueString()
	args := []string{}
	if !plan.UID.IsNull() && !plan.UID.IsUnknown() {
		args = append(args, "-u", strconv.FormatInt(plan.UID.ValueInt64(), 10))
	}
	if !plan.GID.IsNull() && !plan.GID.IsUnknown() {
		args = append(args, "-g", strconv.FormatInt(plan.GID.ValueInt64(), 10))
	}
	if !plan.Home.IsNull() && !plan.Home.IsUnknown() && plan.Home.ValueString() != "" {
		args = append(args, "-d", plan.Home.ValueString())
	}
	if !plan.Shell.IsNull() && plan.Shell.ValueString() != "" {
		args = append(args, "-s", plan.Shell.ValueString())
	}
	if !plan.Comment.IsNull() && plan.Comment.ValueString() != "" {
		args = append(args, "-c", plan.Comment.ValueString())
	}
	if plan.System.ValueBool() {
		args = append(args, "-r")
	}
	if plan.CreateHome.ValueBool() {
		args = append(args, "-m")
	} else {
		args = append(args, "-M")
	}
	groups, err := groupsFromSet(ctx, plan.Groups)
	if err != nil {
		resp.Diagnostics.AddError("Reading groups", err.Error())
		return
	}
	if len(groups) > 0 {
		args = append(args, "-G", strings.Join(groups, ","))
	}
	args = append(args, name)

	if err := runCmd("useradd", args...); err != nil {
		resp.Diagnostics.AddError("useradd failed", err.Error())
		return
	}

	r.refreshState(ctx, &plan, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	plan.ID = plan.Name
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *userResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state userModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	u, err := user.Lookup(state.Name.ValueString())
	if err != nil {
		if _, ok := err.(user.UnknownUserError); ok {
			resp.State.RemoveResource(ctx)
			return
		}
		resp.Diagnostics.AddError("Looking up user", err.Error())
		return
	}
	_ = u
	r.refreshState(ctx, &state, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	state.ID = state.Name
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *userResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state userModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	name := state.Name.ValueString()
	var args []string
	if !plan.UID.Equal(state.UID) && !plan.UID.IsNull() && !plan.UID.IsUnknown() {
		args = append(args, "-u", strconv.FormatInt(plan.UID.ValueInt64(), 10))
	}
	if !plan.GID.Equal(state.GID) && !plan.GID.IsNull() && !plan.GID.IsUnknown() {
		args = append(args, "-g", strconv.FormatInt(plan.GID.ValueInt64(), 10))
	}
	if !plan.Home.Equal(state.Home) && !plan.Home.IsNull() && plan.Home.ValueString() != "" {
		args = append(args, "-d", plan.Home.ValueString())
	}
	if !plan.Shell.Equal(state.Shell) && !plan.Shell.IsNull() {
		args = append(args, "-s", plan.Shell.ValueString())
	}
	if !plan.Comment.Equal(state.Comment) {
		args = append(args, "-c", plan.Comment.ValueString())
	}
	if !plan.Groups.Equal(state.Groups) {
		groups, err := groupsFromSet(ctx, plan.Groups)
		if err != nil {
			resp.Diagnostics.AddError("Reading groups", err.Error())
			return
		}
		args = append(args, "-G", strings.Join(groups, ","))
	}

	if len(args) > 0 {
		args = append(args, name)
		if err := runCmd("usermod", args...); err != nil {
			resp.Diagnostics.AddError("usermod failed", err.Error())
			return
		}
	}

	r.refreshState(ctx, &plan, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	plan.ID = plan.Name
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *userResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state userModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if err := runCmd("userdel", state.Name.ValueString()); err != nil {
		// If the user no longer exists, treat as deleted.
		if _, lookupErr := user.Lookup(state.Name.ValueString()); lookupErr != nil {
			return
		}
		resp.Diagnostics.AddError("userdel failed", err.Error())
		return
	}
}

func (r *userResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("name"), req.ID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), req.ID)...)
}

// refreshState fills computed attributes (uid, gid, home, shell, groups) from
// the actual passwd/group database. Called after create/update/read.
func (r *userResource) refreshState(ctx context.Context, m *userModel, diags *diag.Diagnostics) {
	u, err := user.Lookup(m.Name.ValueString())
	if err != nil {
		diags.AddError("Looking up user after write", err.Error())
		return
	}
	uid, _ := strconv.ParseInt(u.Uid, 10, 64)
	gid, _ := strconv.ParseInt(u.Gid, 10, 64)
	m.UID = types.Int64Value(uid)
	m.GID = types.Int64Value(gid)
	m.Home = types.StringValue(u.HomeDir)

	if shell, err := readLoginShell(u.Username); err == nil {
		m.Shell = types.StringValue(shell)
	} else if m.Shell.IsUnknown() {
		m.Shell = types.StringNull()
	}

	// Only refresh groups if the user provided them; otherwise leave null to
	// avoid a permanent diff against whatever supplementary groups exist.
	if !m.Groups.IsNull() && !m.Groups.IsUnknown() {
		gids, err := u.GroupIds()
		if err == nil {
			names := make([]attr.Value, 0, len(gids))
			for _, g := range gids {
				if g == u.Gid {
					continue
				}
				if grp, err := user.LookupGroupId(g); err == nil {
					names = append(names, types.StringValue(grp.Name))
				}
			}
			set, d := types.SetValue(types.StringType, names)
			diags.Append(d...)
			m.Groups = set
		}
	}
}

func readLoginShell(username string) (string, error) {
	// Read /etc/passwd to get the login shell (not exposed by os/user).
	cmd := exec.Command("getent", "passwd", username)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	fields := strings.Split(strings.TrimRight(string(out), "\n"), ":")
	if len(fields) < 7 {
		return "", fmt.Errorf("unexpected passwd format")
	}
	return fields[6], nil
}
