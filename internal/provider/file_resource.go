package provider

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"syscall"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var (
	_ resource.Resource                = (*fileResource)(nil)
	_ resource.ResourceWithImportState = (*fileResource)(nil)
)

func NewFileResource() resource.Resource { return &fileResource{} }

type fileResource struct{}

type fileModel struct {
	Path    types.String `tfsdk:"path"`
	Content types.String `tfsdk:"content"`
	Mode    types.String `tfsdk:"mode"`
	Owner   types.String `tfsdk:"owner"`
	Group   types.String `tfsdk:"group"`
	ID      types.String `tfsdk:"id"`
}

func (r *fileResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_file"
}

func (r *fileResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Writes a file to a specific location on the local filesystem.",
		Attributes: map[string]schema.Attribute{
			"path": schema.StringAttribute{
				Required:      true,
				Description:   "Absolute path where the file should be written.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"content": schema.StringAttribute{
				Required:    true,
				Description: "File contents.",
			},
			"mode": schema.StringAttribute{
				Optional:    true,
				Computed:    true,
				Description: "Octal file mode (e.g. \"0644\"). Defaults to \"0644\" on create.",
			},
			"owner": schema.StringAttribute{
				Optional:    true,
				Description: "Username of the file owner. Requires privileges to change.",
			},
			"group": schema.StringAttribute{
				Optional:    true,
				Description: "Group name of the file. Requires privileges to change.",
			},
			"id": schema.StringAttribute{
				Computed:      true,
				Description:   "Resource identifier (the file path).",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
		},
	}
}

func parseMode(s string) (fs.FileMode, error) {
	v, err := strconv.ParseUint(s, 8, 32)
	if err != nil {
		return 0, fmt.Errorf("invalid octal mode %q: %w", s, err)
	}
	return fs.FileMode(v) & fs.ModePerm, nil
}

func formatMode(m fs.FileMode) string {
	return fmt.Sprintf("0%o", m&fs.ModePerm)
}

func lookupUID(name string) (int, error) {
	u, err := user.Lookup(name)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(u.Uid)
}

func lookupGID(name string) (int, error) {
	g, err := user.LookupGroup(name)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(g.Gid)
}

func applyOwnership(path string, owner, group string) error {
	if owner == "" && group == "" {
		return nil
	}
	uid, gid := -1, -1
	if owner != "" {
		id, err := lookupUID(owner)
		if err != nil {
			return fmt.Errorf("looking up owner %q: %w", owner, err)
		}
		uid = id
	}
	if group != "" {
		id, err := lookupGID(group)
		if err != nil {
			return fmt.Errorf("looking up group %q: %w", group, err)
		}
		gid = id
	}
	return os.Chown(path, uid, gid)
}

func (r *fileResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan fileModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	modeStr := plan.Mode.ValueString()
	if modeStr == "" {
		modeStr = "0644"
	}
	mode, err := parseMode(modeStr)
	if err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("mode"), "Invalid mode", err.Error())
		return
	}

	target := plan.Path.ValueString()
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		resp.Diagnostics.AddError("Creating parent directory", err.Error())
		return
	}
	if err := os.WriteFile(target, []byte(plan.Content.ValueString()), mode); err != nil {
		resp.Diagnostics.AddError("Writing file", err.Error())
		return
	}
	// WriteFile honors umask on create; force the requested mode explicitly.
	if err := os.Chmod(target, mode); err != nil {
		resp.Diagnostics.AddError("Setting file mode", err.Error())
		return
	}
	if err := applyOwnership(target, plan.Owner.ValueString(), plan.Group.ValueString()); err != nil {
		resp.Diagnostics.AddError("Setting ownership", err.Error())
		return
	}

	plan.Mode = types.StringValue(formatMode(mode))
	plan.ID = plan.Path
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *fileResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state fileModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	target := state.Path.ValueString()
	info, err := os.Stat(target)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			resp.State.RemoveResource(ctx)
			return
		}
		resp.Diagnostics.AddError("Stat failed", err.Error())
		return
	}
	content, err := os.ReadFile(target)
	if err != nil {
		resp.Diagnostics.AddError("Read failed", err.Error())
		return
	}
	state.Content = types.StringValue(string(content))
	state.Mode = types.StringValue(formatMode(info.Mode()))

	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		if !state.Owner.IsNull() {
			if u, err := user.LookupId(strconv.FormatUint(uint64(st.Uid), 10)); err == nil {
				state.Owner = types.StringValue(u.Username)
			}
		}
		if !state.Group.IsNull() {
			if g, err := user.LookupGroupId(strconv.FormatUint(uint64(st.Gid), 10)); err == nil {
				state.Group = types.StringValue(g.Name)
			}
		}
	}
	state.ID = state.Path
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *fileResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan fileModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	modeStr := plan.Mode.ValueString()
	if modeStr == "" {
		modeStr = "0644"
	}
	mode, err := parseMode(modeStr)
	if err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("mode"), "Invalid mode", err.Error())
		return
	}

	target := plan.Path.ValueString()
	if err := os.WriteFile(target, []byte(plan.Content.ValueString()), mode); err != nil {
		resp.Diagnostics.AddError("Writing file", err.Error())
		return
	}
	if err := os.Chmod(target, mode); err != nil {
		resp.Diagnostics.AddError("Setting file mode", err.Error())
		return
	}
	if err := applyOwnership(target, plan.Owner.ValueString(), plan.Group.ValueString()); err != nil {
		resp.Diagnostics.AddError("Setting ownership", err.Error())
		return
	}

	plan.Mode = types.StringValue(formatMode(mode))
	plan.ID = plan.Path
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *fileResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state fileModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if err := os.Remove(state.Path.ValueString()); err != nil && !errors.Is(err, fs.ErrNotExist) {
		resp.Diagnostics.AddError("Removing file", err.Error())
		return
	}
}

func (r *fileResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("path"), req.ID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), req.ID)...)
}
