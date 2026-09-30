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
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var (
	_ resource.Resource                = (*symlinkResource)(nil)
	_ resource.ResourceWithImportState = (*symlinkResource)(nil)
	_ resource.ResourceWithConfigure   = (*symlinkResource)(nil)
	_ resource.ResourceWithModifyPlan  = (*symlinkResource)(nil)
)

// maxTempLinkAttempts bounds the retries when a randomly named temporary
// symlink collides with an existing entry.
const maxTempLinkAttempts = 5

func NewSymlinkResource() resource.Resource { return &symlinkResource{} }

type symlinkResource struct{ rootedResource }

type symlinkModel struct {
	Path   types.String `tfsdk:"path"`
	Target types.String `tfsdk:"target"`
	Owner  types.String `tfsdk:"owner"`
	Group  types.String `tfsdk:"group"`
	ID     types.String `tfsdk:"id"`
}

func (r *symlinkResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_symlink"
}

func (r *symlinkResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Creates a symbolic link at `path` that points to `target`, and optionally sets the `owner` / `group` of the link itself (which requires privileges). " +
			"On destroy, only the link is removed; whatever it points to is never touched.",
		Attributes: map[string]schema.Attribute{
			"path": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "Absolute path of the symbolic link. " +
					"Must be in canonical form (no `.`/`..` segments, duplicate or trailing slashes) and must not be `/`. " +
					"Missing parent directories are created with mode `0755`. Changing this forces a new resource.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
				Validators:    []validator.String{absolutePath()},
			},
			"target": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "Path the link points to, stored verbatim. " +
					"May be absolute or relative; relative targets are resolved by the kernel against the link's directory. " +
					"The target does not need to exist. Must not be empty or contain NUL bytes. " +
					"If the provider's `root_dir` is set, an absolute target is relative to `root_dir` (as in a chroot), and a relative target that leads above `root_dir` is refused at plan time. " +
					"Changing this replaces the link atomically in place.",
				Validators: []validator.String{symlinkTarget()},
			},
			"owner": schema.StringAttribute{
				Optional: true,
				Computed: true,
				MarkdownDescription: "Username or numeric UID that should own the link itself. " +
					"Applied with `lchown`, so the target's ownership is not affected. Requires privileges to change. " +
					"If unset, the owner assigned at creation is kept.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"group": schema.StringAttribute{
				Optional: true,
				Computed: true,
				MarkdownDescription: "Group name or numeric GID of the link itself. Applied with `lchown`, so the target's ownership is not affected. " +
					"Requires privileges to change. If unset, the group assigned at creation is kept.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Resource identifier (equal to `path`).",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
		},
	}
}

// ModifyPlan refuses, when root_dir is set, a target that would lead out of
// the root, so that the mistake is reported by plan rather than apply.
func (r *symlinkResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.Plan.Raw.IsNull() || r.root().isHost() {
		return
	}
	var plan symlinkModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() || plan.Path.IsUnknown() || plan.Target.IsUnknown() {
		return
	}
	if err := r.root().checkSymlinkTargetInRoot(plan.Path.ValueString(), plan.Target.ValueString()); err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("target"), "Invalid target", capitalize(err.Error())+".")
	}
}

func (r *symlinkResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan symlinkModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	target := plan.Target.ValueString()
	// Config validation already enforces these; re-check as defense in depth.
	if err := validateAbsolutePath(plan.Path.ValueString()); err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("path"), "Invalid path", capitalize(err.Error())+".")
		return
	}
	if err := r.validateTarget(plan.Path.ValueString(), target); err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("target"), "Invalid target", capitalize(err.Error())+".")
		return
	}
	link, diags := resolvePathAttr(r.root(), plan.Path.ValueString(), false)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	if err := r.checkResolvedTarget(link, target); err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("target"), "Invalid target", capitalize(err.Error())+".")
		return
	}

	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		resp.Diagnostics.AddError("Creating parent directory", err.Error())
		return
	}
	// Owner and group are unknown when not configured; ValueString returns ""
	// for unknown values, which means "leave unchanged".
	if err := replaceSymlink(link, target, plan.Owner.ValueString(), plan.Group.ValueString()); err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("path"), "Creating symlink", explainImmutable(err, filepath.Dir(link)).Error())
		return
	}

	found, diags := readSymlink(link, &plan)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	if !found {
		resp.Diagnostics.AddError("Creating symlink",
			fmt.Sprintf("Symlink %q disappeared immediately after creation.", link))
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *symlinkResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state symlinkModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	link, diags := resolvePathAttr(r.root(), state.Path.ValueString(), false)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	found, diags := readSymlink(link, &state)
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

func (r *symlinkResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan symlinkModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	target := plan.Target.ValueString()
	if err := r.validateTarget(plan.Path.ValueString(), target); err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("target"), "Invalid target", capitalize(err.Error())+".")
		return
	}
	link, diags := resolvePathAttr(r.root(), plan.Path.ValueString(), false)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	if err := r.checkResolvedTarget(link, target); err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("target"), "Invalid target", capitalize(err.Error())+".")
		return
	}

	// Owner and group are always known here (configured, or carried over from
	// state by UseStateForUnknown). Both helpers only chown what differs from
	// the link on disk, so an unrelated update needs no extra privileges.
	owner, group := knownString(plan.Owner), knownString(plan.Group)
	current, err := os.Readlink(link)
	switch {
	case err == nil && current == target:
		err = setLinkOwnership(link, owner, group)
	case err == nil, errors.Is(err, fs.ErrNotExist):
		err = replaceSymlink(link, target, owner, group)
	case errors.Is(err, syscall.EINVAL):
		err = fmt.Errorf("path %q exists but is not a symlink", link)
	}
	if err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("path"), "Updating symlink", explainImmutable(err, filepath.Dir(link)).Error())
		return
	}

	found, diags := readSymlink(link, &plan)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	if !found {
		resp.Diagnostics.AddError("Updating symlink",
			fmt.Sprintf("Symlink %q disappeared during update.", link))
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *symlinkResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state symlinkModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	// State is not validated by the schema; never act on an invalid path.
	if err := validateAbsolutePath(state.Path.ValueString()); err != nil {
		resp.Diagnostics.AddError("Refusing to remove symlink", capitalize(err.Error())+".")
		return
	}
	link, diags := resolvePathAttr(r.root(), state.Path.ValueString(), false)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	info, err := os.Lstat(link)
	if errors.Is(err, fs.ErrNotExist) {
		return
	}
	if err != nil {
		resp.Diagnostics.AddError("Removing symlink", err.Error())
		return
	}
	if info.Mode()&fs.ModeSymlink == 0 {
		// Something else replaced the link; it is not ours to delete.
		resp.Diagnostics.AddWarning("Symlink already replaced",
			fmt.Sprintf("Path %q is no longer a symlink and was left untouched.", link))
		return
	}
	// unlink never follows symlinks, so only the link itself is deleted, and
	// unlike os.Remove it never falls back to rmdir if a directory has been
	// swapped in since the Lstat above.
	if err := syscall.Unlink(link); err != nil && !errors.Is(err, fs.ErrNotExist) {
		resp.Diagnostics.AddError("Removing symlink", explainImmutable(&fs.PathError{Op: "unlink", Path: link, Err: err}, filepath.Dir(link)).Error())
	}
}

func (r *symlinkResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	if err := validateAbsolutePath(req.ID); err != nil {
		resp.Diagnostics.AddError("Invalid import ID",
			fmt.Sprintf("Import ID must be the absolute path of the symlink: %s.", err))
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("path"), req.ID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), req.ID)...)
}

// validateTarget checks the symlink target of the link at the managed path
// link, including that it stays inside root_dir.
func (r *symlinkResource) validateTarget(link, target string) error {
	if err := validateSymlinkTarget(target); err != nil {
		return err
	}
	return r.root().checkSymlinkTargetInRoot(link, target)
}

// checkResolvedTarget repeats the root_dir check of validateTarget for the
// link's real location in the tree, link being the resolved host path.
// Symlinks among the link's ancestors may put it at a different depth than
// its configured path suggests ("/a/b/link" with "/a/b" a link to "/" is
// "/link"), so that a relative target that looked safe leads above the root.
func (r *symlinkResource) checkResolvedTarget(link, target string) error {
	inRoot, err := r.root().inRoot(link)
	if err != nil {
		return err
	}
	return r.root().checkSymlinkTargetInRoot(inRoot, target)
}

// replaceSymlink atomically points link at target: a new symlink is created
// under a temporary name in the same directory, given the requested
// ownership, and renamed over link. Readers therefore see either the old or
// the new link, never a missing one. An existing entry at link that is not a
// symlink is refused rather than clobbered. Empty owner or group values leave
// the corresponding attribute as assigned on creation.
func replaceSymlink(link, target, owner, group string) error {
	if info, err := os.Lstat(link); err == nil {
		if info.Mode()&fs.ModeSymlink == 0 {
			return fmt.Errorf("path %q exists but is not a symlink", link)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}

	tmp, err := createTempSymlink(link, target)
	if err != nil {
		return err
	}
	if err := setLinkOwnership(tmp, owner, group); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("setting ownership: %w", err)
	}
	if err := os.Rename(tmp, link); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// createTempSymlink creates a symlink to target with a random hidden name in
// the directory of link and returns its path.
func createTempSymlink(link, target string) (string, error) {
	dir, base := filepath.Split(link)
	var err error
	for range maxTempLinkAttempts {
		tmp := filepath.Join(dir, "."+base+".sysutils-tmp-"+randomID())
		if err = os.Symlink(target, tmp); err == nil {
			return tmp, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return "", err
		}
	}
	return "", fmt.Errorf("creating temporary symlink next to %q: %w", link, err)
}

// setLinkOwnership lchowns link to owner and group, skipping IDs that already
// match so that no privileges are needed when nothing changes. Empty values
// leave the corresponding attribute unchanged.
func setLinkOwnership(link, owner, group string) error {
	if owner == "" && group == "" {
		return nil
	}
	uid, gid, err := resolveOwnership(owner, group)
	if err != nil {
		return err
	}
	info, err := os.Lstat(link)
	if err != nil {
		return err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("unable to determine ownership of %q on this platform", link)
	}
	if uid >= 0 && uint64(uid) == uint64(st.Uid) {
		uid = -1
	}
	if gid >= 0 && uint64(gid) == uint64(st.Gid) {
		gid = -1
	}
	if uid == -1 && gid == -1 {
		return nil
	}
	return os.Lchown(link, uid, gid)
}

// readSymlink refreshes m's target, owner, group and id from the symlink at
// link, without following it. Configured owner and group values equivalent
// to the actual ones (e.g. a numeric UID vs its username) are preserved to
// avoid spurious diffs. It reports found=false if link does not exist or is
// no longer a symlink, so that it is planned for re-creation.
func readSymlink(link string, m *symlinkModel) (found bool, diags diag.Diagnostics) {
	info, err := os.Lstat(link)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, diags
		}
		diags.AddError("Stat failed", err.Error())
		return false, diags
	}
	if info.Mode()&fs.ModeSymlink == 0 {
		return false, diags
	}

	target, err := os.Readlink(link)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.EINVAL) {
			// Removed or replaced between Lstat and Readlink.
			return false, diags
		}
		diags.AddError("Reading symlink", err.Error())
		return false, diags
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		diags.AddError("Stat failed", fmt.Sprintf("Unable to determine ownership of %q on this platform.", link))
		return true, diags
	}

	m.Target = types.StringValue(target)
	m.Owner = types.StringValue(reconcileOwner(knownString(m.Owner), st.Uid))
	m.Group = types.StringValue(reconcileGroup(knownString(m.Group), st.Gid))
	m.ID = m.Path
	return true, diags
}
