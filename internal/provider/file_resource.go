package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"unicode/utf8"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var (
	_ resource.Resource                = (*fileResource)(nil)
	_ resource.ResourceWithImportState = (*fileResource)(nil)
)

const defaultFileMode = "0644"

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
				Validators:    []validator.String{absolutePath()},
			},
			"content": schema.StringAttribute{
				Required:    true,
				Description: "File contents.",
			},
			"mode": schema.StringAttribute{
				Optional:    true,
				Computed:    true,
				Default:     stringdefault.StaticString(defaultFileMode),
				Description: "Octal file mode (e.g. \"0644\"). Applied explicitly, so it is not affected by the umask. Defaults to \"" + defaultFileMode + "\".",
				Validators:  []validator.String{octalMode()},
			},
			"owner": schema.StringAttribute{
				Optional:      true,
				Computed:      true,
				Description:   "Username (or numeric UID) of the file owner. Requires privileges to change. Defaults to the owner assigned on creation.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"group": schema.StringAttribute{
				Optional:      true,
				Computed:      true,
				Description:   "Group name (or numeric GID) of the file. Requires privileges to change. Defaults to the group assigned on creation.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"id": schema.StringAttribute{
				Computed:      true,
				Description:   "Resource identifier (the file path).",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
		},
	}
}

func (r *fileResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan fileModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	target := plan.Path.ValueString()
	// Config validation already enforces this; re-check as defense in depth.
	if err := validateAbsolutePath(target); err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("path"), "Invalid path", capitalize(err.Error())+".")
		return
	}
	mode, err := parseMode(plan.Mode.ValueString())
	if err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("mode"), "Invalid mode", err.Error())
		return
	}

	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		resp.Diagnostics.AddError("Creating parent directory", err.Error())
		return
	}
	// Owner and group are unknown when not configured; ValueString returns ""
	// for unknown values, meaning "leave unchanged".
	if err := writeFile(target, plan.Content.ValueString(), mode, plan.Owner.ValueString(), plan.Group.ValueString()); err != nil {
		resp.Diagnostics.AddError("Writing file", err.Error())
		return
	}

	found, diags := readFile(target, &plan)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	if !found {
		resp.Diagnostics.AddError("Writing file",
			fmt.Sprintf("File %q disappeared immediately after creation.", target))
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *fileResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state fileModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	found, diags := readFile(state.Path.ValueString(), &state)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	if !found {
		resp.State.RemoveResource(ctx)
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *fileResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state fileModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	target := plan.Path.ValueString()
	mode, err := parseMode(plan.Mode.ValueString())
	if err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("mode"), "Invalid mode", err.Error())
		return
	}

	// Only chown what actually changed, so that an unrelated update does not
	// require the privileges needed to (re)assign ownership.
	var owner, group string
	if !plan.Owner.IsUnknown() && !plan.Owner.Equal(state.Owner) {
		owner = plan.Owner.ValueString()
	}
	if !plan.Group.IsUnknown() && !plan.Group.Equal(state.Group) {
		group = plan.Group.ValueString()
	}
	if err := writeFile(target, plan.Content.ValueString(), mode, owner, group); err != nil {
		resp.Diagnostics.AddError("Writing file", err.Error())
		return
	}

	found, diags := readFile(target, &plan)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	if !found {
		resp.Diagnostics.AddError("Writing file",
			fmt.Sprintf("File %q disappeared during update.", target))
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *fileResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state fileModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	target := state.Path.ValueString()
	// State is not validated by the schema; never act on an invalid path.
	if err := validateAbsolutePath(target); err != nil {
		resp.Diagnostics.AddError("Refusing to remove file", capitalize(err.Error())+".")
		return
	}
	// unlink never follows symlinks and never removes directories, unlike
	// os.Remove, which falls back to rmdir.
	if err := syscall.Unlink(target); err != nil && !errors.Is(err, fs.ErrNotExist) {
		resp.Diagnostics.AddError("Removing file", (&fs.PathError{Op: "unlink", Path: target, Err: err}).Error())
		return
	}
}

func (r *fileResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	if err := validateAbsolutePath(req.ID); err != nil {
		resp.Diagnostics.AddError("Invalid import ID",
			fmt.Sprintf("Import ID must be the absolute path of the file: %s.", err))
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("path"), req.ID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), req.ID)...)
}

// writeFile writes content to target and applies mode and ownership. Empty
// owner or group values leave the corresponding attribute unchanged.
//
// A symlink or other non-regular file at target is refused. The file is
// opened with O_NOFOLLOW and all changes go through that descriptor, so it
// cannot be swapped for a symlink between the check and the write. Ownership
// and mode are applied before the new content is written, so content never
// becomes visible under a more permissive mode or previous owner.
func writeFile(target, content string, mode fs.FileMode, owner, group string) error {
	if info, err := os.Lstat(target); err == nil {
		if err := checkRegularFile(target, info); err != nil {
			return err
		}
	}
	// A newly created file starts out private; its final mode is set below.
	f, err := openNoFollow(target, os.O_WRONLY|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if err := checkRegularFile(target, info); err != nil {
		return err
	}
	if err := setOwnershipAndMode(f, owner, group, mode); err != nil {
		return err
	}
	if err := f.Truncate(0); err != nil {
		return err
	}
	if _, err := f.WriteString(content); err != nil {
		return err
	}
	return f.Close()
}

// checkRegularFile returns a descriptive error unless info describes a
// regular file.
func checkRegularFile(target string, info fs.FileInfo) error {
	if info.Mode()&fs.ModeSymlink != 0 {
		return symlinkRefusedError(target)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("path %q exists but is not a regular file", target)
	}
	return nil
}

// readFile refreshes m's content, mode, owner, group and id from the file at
// target. Configured values that are equivalent to the actual ones (e.g. "644"
// vs "0644", or a numeric UID vs its username) are preserved to avoid spurious
// diffs. It reports found=false if target does not exist.
func readFile(target string, m *fileModel) (found bool, diags diag.Diagnostics) {
	info, err := os.Lstat(target)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, diags
		}
		diags.AddError("Stat failed", err.Error())
		return false, diags
	}
	if err := checkRegularFile(target, info); err != nil {
		diags.AddAttributeError(path.Root("path"), "Not a regular file", capitalize(err.Error())+".")
		return true, diags
	}

	// Open without following symlinks and re-check the type on the
	// descriptor, so a path swapped after Lstat (e.g. for a FIFO that would
	// block, or a symlink to a sensitive file) is never read.
	f, err := openNoFollow(target, os.O_RDONLY, 0)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// Removed between Lstat and Open.
			return false, diags
		}
		diags.AddError("Read failed", err.Error())
		return false, diags
	}
	defer func() { _ = f.Close() }()
	if info, err = f.Stat(); err != nil {
		diags.AddError("Stat failed", err.Error())
		return false, diags
	}
	if err := checkRegularFile(target, info); err != nil {
		diags.AddAttributeError(path.Root("path"), "Not a regular file", capitalize(err.Error())+".")
		return true, diags
	}
	content, err := io.ReadAll(f)
	if err != nil {
		diags.AddError("Read failed", err.Error())
		return false, diags
	}
	// Terraform strings must be valid UTF-8; binary files cannot be managed.
	if !utf8.Valid(content) {
		diags.AddAttributeError(path.Root("content"), "Unsupported file content",
			fmt.Sprintf("File %q does not contain valid UTF-8 text and cannot be represented in the content attribute.", target))
		return true, diags
	}
	m.Content = types.StringValue(string(content))

	m.Mode = types.StringValue(reconcileMode(knownString(m.Mode), info.Mode()))
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		diags.AddError("Stat failed", fmt.Sprintf("Unable to determine ownership of %q on this platform.", target))
		return true, diags
	}
	m.Owner = types.StringValue(reconcileOwner(knownString(m.Owner), st.Uid))
	m.Group = types.StringValue(reconcileGroup(knownString(m.Group), st.Gid))
	m.ID = m.Path
	return true, diags
}
