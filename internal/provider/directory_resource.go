package provider

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var (
	_ resource.Resource                   = (*directoryResource)(nil)
	_ resource.ResourceWithImportState    = (*directoryResource)(nil)
	_ resource.ResourceWithValidateConfig = (*directoryResource)(nil)
	_ resource.ResourceWithConfigure      = (*directoryResource)(nil)
)

const (
	defaultDirectoryMode          = "0755"
	defaultDirectoryCreateParents = true
	defaultDirectoryForceDestroy  = false
	defaultDirectoryRecursive     = false
)

func NewDirectoryResource() resource.Resource { return &directoryResource{} }

type directoryResource struct{ rootedResource }

type directoryModel struct {
	Path          types.String `tfsdk:"path"`
	Mode          types.String `tfsdk:"mode"`
	Owner         types.String `tfsdk:"owner"`
	Group         types.String `tfsdk:"group"`
	CreateParents types.Bool   `tfsdk:"create_parents"`
	ForceDestroy  types.Bool   `tfsdk:"force_destroy"`
	ID            types.String `tfsdk:"id"`

	RecursiveOwner       types.Bool   `tfsdk:"recursive_owner"`
	RecursiveMode        types.Bool   `tfsdk:"recursive_mode"`
	FileMode             types.String `tfsdk:"file_mode"`
	NonconformingEntries types.Int64  `tfsdk:"nonconforming_entries"`
}

// treeSpec returns the recursive enforcement settings of m. dirMode and
// fileMode are passed explicitly because Read compares the tree against the
// previously applied modes rather than the freshly refreshed ones.
func (m *directoryModel) treeSpec(dirMode, fileMode string) (treeSpec, error) {
	return newTreeSpec(m.RecursiveOwner.ValueBool(), m.RecursiveMode.ValueBool(), dirMode, fileMode)
}

func (r *directoryResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_directory"
}

func (r *directoryResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Ensures a directory exists at `path` with the requested `mode` and, optionally, `owner` / `group` (which require privileges). " +
			"On update, mode and ownership are reconciled in place. " +
			"On destroy, the directory is removed — but only if it is empty, unless `force_destroy` is set.",
		Attributes: map[string]schema.Attribute{
			"path": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "Absolute path of the directory. " +
					"Must be in canonical form (no `.`/`..` segments, duplicate or trailing slashes) and must not be `/`. " +
					"Changing this forces a new resource.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
				Validators:    []validator.String{absolutePath()},
			},
			"mode": schema.StringAttribute{
				Optional: true,
				Computed: true,
				Default:  stringdefault.StaticString(defaultDirectoryMode),
				MarkdownDescription: "Octal mode with 3 or 4 digits, optionally with a leading zero, such as `\"0755\"`, `\"750\"` or `\"1777\"`. " +
					"Setuid, setgid and sticky bits are supported. " +
					"The mode is applied with an explicit `chmod`, so the process umask does not affect it. Defaults to `\"0755\"`.",
				Validators: []validator.String{octalMode()},
			},
			"owner": schema.StringAttribute{
				Optional: true,
				Computed: true,
				MarkdownDescription: "Username or numeric UID that should own the directory. Requires privileges to change. " +
					"If unset, the owner assigned at creation (normally the user running Terraform) is kept.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"group": schema.StringAttribute{
				Optional: true,
				Computed: true,
				MarkdownDescription: "Group name or numeric GID of the directory. Requires privileges to change. " +
					"If unset, the group assigned at creation is kept.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"create_parents": schema.BoolAttribute{
				Optional: true,
				Computed: true,
				Default:  booldefault.StaticBool(defaultDirectoryCreateParents),
				MarkdownDescription: "Create missing parent directories, like `mkdir -p`. " +
					"Parents created this way get mode `0755` (subject to the umask) and are not removed on destroy. " +
					"When `false`, creation fails if the parent does not exist. Defaults to `true`.",
			},
			"force_destroy": schema.BoolAttribute{
				Optional: true,
				Computed: true,
				Default:  booldefault.StaticBool(defaultDirectoryForceDestroy),
				MarkdownDescription: "Delete the directory and everything in it on destroy. " +
					"When `false`, destroying a non-empty directory fails with an error. " +
					"The recursive delete refuses paths with a symlink in any component, never follows symlinks inside the tree, and never crosses into other mounted filesystems. " +
					"Cannot be enabled for protected system directories such as `/etc`, `/usr` or `/home`. Defaults to `false`.",
			},
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Resource identifier (equal to `path`).",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"recursive_owner": schema.BoolAttribute{
				Optional: true,
				Computed: true,
				Default:  booldefault.StaticBool(defaultDirectoryRecursive),
				MarkdownDescription: "Give every entry below the directory (files, subdirectories, symlinks and special files) the directory's own owner and group, like `chown -R`. " +
					"If `owner` or `group` is not configured, the directory's current value is used. " +
					"Symlinks inside the tree have their own ownership changed; they are never followed. Defaults to `false`.",
			},
			"recursive_mode": schema.BoolAttribute{
				Optional: true,
				Computed: true,
				Default:  booldefault.StaticBool(defaultDirectoryRecursive),
				MarkdownDescription: "Apply `mode` to every subdirectory and `file_mode` to every regular file below the directory, like `chmod -R`. " +
					"Symlinks, FIFOs, sockets and device nodes are left alone. Defaults to `false`.",
			},
			"file_mode": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "Octal mode for regular files inside the tree, in the same format as `mode`. " +
					"Only valid with `recursive_mode = true`. " +
					"If unset, files get `mode`, exactly as with `chmod -R` — which makes every file executable if `mode` is a typical directory mode such as `\"0755\"`, so you usually want to set it.",
				Validators: []validator.String{octalMode()},
			},
			"nonconforming_entries": schema.Int64Attribute{
				Computed: true,
				MarkdownDescription: "Number of entries below the directory whose ownership or mode did not match `recursive_owner` / `recursive_mode` at the last refresh. " +
					"It is always `0` after apply and whenever recursion is disabled; a non-zero value is drift inside the tree (see [Drift Detection](#drift-detection)).",
				PlanModifiers: []planmodifier.Int64{zeroAfterApply{}},
			},
		},
	}
}

// ValidateConfig rejects force_destroy on protected system directories at
// plan time, so the mistake surfaces long before a destroy is attempted.
func (r *directoryResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var cfg directoryModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if !cfg.FileMode.IsNull() && !cfg.RecursiveMode.IsUnknown() && !cfg.RecursiveMode.ValueBool() {
		resp.Diagnostics.AddAttributeError(path.Root("file_mode"), "file_mode requires recursive_mode",
			"file_mode only applies to files inside the directory and has no effect unless recursive_mode = true.")
	}
	if !cfg.ForceDestroy.ValueBool() || cfg.Path.IsNull() || cfg.Path.IsUnknown() {
		return
	}
	p := cfg.Path.ValueString()
	if validateAbsolutePath(p) != nil {
		// Reported by the path validator.
		return
	}
	if err := validateRecursivelyRemovable(p); err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("force_destroy"), "Unsafe force_destroy", capitalize(err.Error())+".")
	}
}

func (r *directoryResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan directoryModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	// Config validation already enforces this; re-check as defense in depth.
	if err := validateAbsolutePath(plan.Path.ValueString()); err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("path"), "Invalid path", capitalize(err.Error())+".")
		return
	}
	target, diags := resolvePathAttr(r.root(), plan.Path.ValueString(), false)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	mode, err := parseMode(plan.Mode.ValueString())
	if err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("mode"), "Invalid mode", err.Error())
		return
	}

	if err := ensureDirectory(target, plan.CreateParents.ValueBool()); err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("path"), "Creating directory", err.Error())
		return
	}
	// Mkdir honors the umask, so the requested mode is always applied
	// explicitly. Owner and group are unknown when not configured;
	// ValueString returns "" for unknown values, meaning "leave unchanged".
	if err := applyDirectoryAttributes(target, mode, plan.Owner.ValueString(), plan.Group.ValueString()); err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("path"), "Setting directory attributes", err.Error())
		return
	}
	resp.Diagnostics.Append(conformDirectoryTree(ctx, target, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	found, diags := readDirectory(target, &plan)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	if !found {
		resp.Diagnostics.AddError("Creating directory",
			fmt.Sprintf("Directory %q disappeared immediately after creation.", target))
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *directoryResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state directoryModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	// The tree is compared against the modes last applied, not the ones
	// refreshed from the directory itself: drift of the directory's own mode
	// is reported through the mode attribute.
	appliedMode, appliedFileMode := knownString(state.Mode), knownString(state.FileMode)

	target, diags := resolvePathAttr(r.root(), state.Path.ValueString(), false)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	found, diags := readDirectory(target, &state)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	if !found {
		resp.State.RemoveResource(ctx)
		return
	}

	// Imported resources have no value for the provider-only settings yet.
	if state.CreateParents.IsNull() {
		state.CreateParents = types.BoolValue(defaultDirectoryCreateParents)
	}
	if state.ForceDestroy.IsNull() {
		state.ForceDestroy = types.BoolValue(defaultDirectoryForceDestroy)
	}
	if state.RecursiveOwner.IsNull() {
		state.RecursiveOwner = types.BoolValue(defaultDirectoryRecursive)
	}
	if state.RecursiveMode.IsNull() {
		state.RecursiveMode = types.BoolValue(defaultDirectoryRecursive)
	}

	spec, err := state.treeSpec(appliedMode, appliedFileMode)
	if err != nil {
		resp.Diagnostics.AddError("Invalid mode in state", err.Error())
		return
	}
	n, err := conformTree(ctx, target, spec, false)
	if err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("path"), "Inspecting directory contents", capitalize(err.Error())+".")
		return
	}
	state.NonconformingEntries = types.Int64Value(n)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *directoryResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state directoryModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	target, diags := resolvePathAttr(r.root(), plan.Path.ValueString(), false)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
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
	if err := applyDirectoryAttributes(target, mode, owner, group); err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("path"), "Updating directory", err.Error())
		return
	}
	// The tree is walked on every update: a non-zero nonconforming_entries
	// may be the only reason for this update.
	resp.Diagnostics.Append(conformDirectoryTree(ctx, target, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	found, diags := readDirectory(target, &plan)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	if !found {
		resp.Diagnostics.AddError("Updating directory",
			fmt.Sprintf("Directory %q disappeared during update.", target))
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *directoryResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state directoryModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	// State is not validated by the schema; never act on an empty, relative,
	// non-canonical or root path read from it.
	if err := validateAbsolutePath(state.Path.ValueString()); err != nil {
		resp.Diagnostics.AddError("Refusing to remove directory", capitalize(err.Error())+".")
		return
	}
	target, diags := resolvePathAttr(r.root(), state.Path.ValueString(), false)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	info, err := os.Lstat(target)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return
	case err != nil:
		resp.Diagnostics.AddError("Removing directory", err.Error())
		return
	case !info.IsDir():
		// A symlink or file now occupies the path; it is not ours to delete.
		resp.Diagnostics.AddWarning("Directory already replaced",
			fmt.Sprintf("Path %q is no longer a directory and was left untouched.", target))
		return
	}

	if state.ForceDestroy.ValueBool() {
		// removeAllNoFollow checks the host path; the path inside root_dir
		// is held to the same list of protected directories. Symlinks in
		// the tree may have redirected the configured path ("/a/etc" with
		// "/a" a link to "/" is "/etc"), so both it and the path it
		// resolved to are checked.
		inRoot, err := r.root().inRoot(target)
		if err == nil {
			err = validateRecursivelyRemovable(state.Path.ValueString())
		}
		if err == nil {
			err = validateRecursivelyRemovable(inRoot)
		}
		if err != nil {
			resp.Diagnostics.AddError("Removing directory", capitalize(err.Error())+".")
			return
		}
		if err := removeAllNoFollow(target); err != nil {
			resp.Diagnostics.AddError("Removing directory", capitalize(err.Error())+".")
		}
		return
	}

	// rmdir never follows symlinks and only removes empty directories.
	err = syscall.Rmdir(target)
	switch {
	case err == nil, errors.Is(err, fs.ErrNotExist):
		return
	case errors.Is(err, syscall.ENOTEMPTY), errors.Is(err, syscall.EEXIST):
		resp.Diagnostics.AddError("Directory not empty",
			fmt.Sprintf("Directory %q is not empty and force_destroy is false. "+
				"Remove its contents manually, or set force_destroy = true and apply before destroying "+
				"to delete the directory recursively.", target))
	case errors.Is(err, syscall.ENOTDIR):
		resp.Diagnostics.AddWarning("Directory already replaced",
			fmt.Sprintf("Path %q is no longer a directory and was left untouched.", target))
	default:
		resp.Diagnostics.AddError("Removing directory", (&fs.PathError{Op: "rmdir", Path: target, Err: err}).Error())
	}
}

func (r *directoryResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	if err := validateAbsolutePath(req.ID); err != nil {
		resp.Diagnostics.AddError("Invalid import ID",
			fmt.Sprintf("Import ID must be the absolute path of the directory: %s.", err))
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("path"), req.ID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), req.ID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("create_parents"), defaultDirectoryCreateParents)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("force_destroy"), defaultDirectoryForceDestroy)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("recursive_owner"), defaultDirectoryRecursive)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("recursive_mode"), defaultDirectoryRecursive)...)
}

// conformDirectoryTree applies the recursive ownership and mode settings of m
// to everything below target and records the result in m. The directory's own
// attributes must already have been applied, since the tree takes its owner
// and group from the directory.
func conformDirectoryTree(ctx context.Context, target string, m *directoryModel) diag.Diagnostics {
	var diags diag.Diagnostics
	spec, err := m.treeSpec(m.Mode.ValueString(), knownString(m.FileMode))
	if err != nil {
		diags.AddAttributeError(path.Root("file_mode"), "Invalid mode", err.Error())
		return diags
	}
	if _, err := conformTree(ctx, target, spec, true); err != nil {
		diags.AddAttributeError(path.Root("path"), "Applying attributes to directory contents", capitalize(err.Error())+".")
		return diags
	}
	// Every entry found was corrected (or the walk failed above).
	m.NonconformingEntries = types.Int64Value(0)
	return diags
}

// zeroAfterApply plans nonconforming_entries as 0: after any apply the tree
// conforms. A refreshed non-zero value therefore differs from the plan and
// makes drift inside the tree trigger an update.
type zeroAfterApply struct{}

var _ planmodifier.Int64 = zeroAfterApply{}

func (zeroAfterApply) Description(context.Context) string {
	return "Planned as 0, the value after apply."
}

func (m zeroAfterApply) MarkdownDescription(ctx context.Context) string { return m.Description(ctx) }

func (zeroAfterApply) PlanModifyInt64(_ context.Context, req planmodifier.Int64Request, resp *planmodifier.Int64Response) {
	if req.Plan.Raw.IsNull() {
		return
	}
	resp.PlanValue = types.Int64Value(0)
}

// ensureDirectory makes sure target exists as a directory. An existing
// directory is accepted as is; an existing non-directory is an error. When
// createParents is false, a missing parent directory is an error.
func ensureDirectory(target string, createParents bool) error {
	err := checkIsDirectory(target)
	if err == nil || !errors.Is(err, fs.ErrNotExist) {
		return err
	}

	if createParents {
		err = os.MkdirAll(target, 0o755)
	} else {
		err = os.Mkdir(target, 0o755)
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("parent directory %q does not exist and create_parents is false", filepath.Dir(target))
		}
	}
	if errors.Is(err, fs.ErrExist) {
		// Lost a race with a concurrent creator; accept it only if it is a directory.
		return checkIsDirectory(target)
	}
	return err
}

// checkIsDirectory returns nil if target is a directory, an error wrapping
// fs.ErrNotExist if it does not exist, and a descriptive error otherwise. A
// symlink at target is refused even if it points to a directory.
func checkIsDirectory(target string) error {
	info, err := os.Lstat(target)
	if err != nil {
		return err
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		return symlinkRefusedError(target)
	}
	if !info.IsDir() {
		return fmt.Errorf("path %q exists but is not a directory", target)
	}
	return nil
}

// applyDirectoryAttributes sets the mode and, if non-empty, the owner and
// group of the directory at target. The directory is opened without following
// a symlink at target and modified through that descriptor, so it cannot be
// swapped for a symlink (or anything else) between the check and the change.
func applyDirectoryAttributes(target string, mode fs.FileMode, owner, group string) error {
	f, err := openNoFollow(target, os.O_RDONLY|syscall.O_DIRECTORY, 0)
	if err != nil {
		if errors.Is(err, syscall.ENOTDIR) {
			return fmt.Errorf("path %q exists but is not a directory", target)
		}
		return err
	}
	defer func() { _ = f.Close() }()
	return setOwnershipAndMode(f, owner, group, mode)
}

// readDirectory refreshes m's mode, owner, group and id from the directory at
// target, which is not followed if it is a symlink. Configured values that are equivalent to the actual ones (e.g. "755"
// vs "0755", or a numeric UID vs its username) are preserved to avoid spurious
// diffs. It reports found=false if target does not exist.
func readDirectory(target string, m *directoryModel) (found bool, diags diag.Diagnostics) {
	info, err := os.Lstat(target)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, diags
		}
		diags.AddError("Stat failed", err.Error())
		return false, diags
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		diags.AddAttributeError(path.Root("path"), "Not a directory",
			fmt.Sprintf("Path %q is a symbolic link, not a directory. Manage the real directory path instead.", target))
		return true, diags
	}
	if !info.IsDir() {
		diags.AddAttributeError(path.Root("path"), "Not a directory",
			fmt.Sprintf("Path %q exists but is not a directory.", target))
		return true, diags
	}

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

// knownString returns the value of s, or "" if it is null or unknown.
func knownString(s types.String) string {
	if s.IsNull() || s.IsUnknown() {
		return ""
	}
	return s.ValueString()
}
