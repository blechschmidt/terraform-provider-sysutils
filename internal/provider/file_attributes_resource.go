package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework-validators/setvalidator"
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
	_ resource.Resource                = (*fileAttributesResource)(nil)
	_ resource.ResourceWithConfigure   = (*fileAttributesResource)(nil)
	_ resource.ResourceWithImportState = (*fileAttributesResource)(nil)
)

const (
	defaultFileAttrsExclusive = false
	// fileAttrsPrivateOriginal is the private state key of the inode flags
	// the path had before the resource was created, restored on destroy.
	fileAttrsPrivateOriginal = "original_flags"
)

func NewFileAttributesResource() resource.Resource { return &fileAttributesResource{} }

type fileAttributesResource struct{ rootedResource }

type fileAttributesModel struct {
	Path       types.String `tfsdk:"path"`
	Attributes types.Set    `tfsdk:"attributes"`
	Exclusive  types.Bool   `tfsdk:"exclusive"`
	ID         types.String `tfsdk:"id"`
}

func (r *fileAttributesResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_file_attributes"
}

func (r *fileAttributesResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	flagList := make([]string, len(fileAttrFlags))
	for i, f := range fileAttrFlags {
		flagList[i] = fmt.Sprintf("`%s` (%s)", f.letter, f.name)
	}
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages the inode flags of an existing file or directory, which `chattr` sets and `lsattr` shows, such as immutable (`i`), append only (`a`) or no dump (`d`). " +
			"The flags are read and written directly with the `FS_IOC_GETFLAGS` and `FS_IOC_SETFLAGS` ioctls, so no `chattr` binary is needed. " +
			"They are supported by ext2/3/4, XFS, Btrfs and some other file systems, but not, or only partly, by tmpfs and overlayfs. " +
			"On destroy, the flags the resource managed are restored to what they were before it was created.",
		Attributes: map[string]schema.Attribute{
			"path": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "Absolute path of an existing regular file or directory. " +
					"Must be in canonical form (no `.`/`..` segments, duplicate or trailing slashes) and must not be `/`. " +
					"A symlink at the path is refused, never followed. Changing this forces a new resource.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
				Validators:    []validator.String{absolutePath()},
			},
			"attributes": schema.SetAttribute{
				ElementType: types.StringType,
				Required:    true,
				MarkdownDescription: "Flags to set, each a single `chattr` letter: " + strings.Join(flagList, ", ") + ". " +
					"Setting or clearing `i` or `a` needs the `CAP_LINUX_IMMUTABLE` capability (root); the others need the file's owner or `CAP_FOWNER`. " +
					"Flags the file system doesn't support are reported as errors. " +
					"A flag removed from the list is restored to what it was before the resource was created.",
				Validators: []validator.Set{setvalidator.ValueStringsAre(stringCheck("inode flag", func(s string) error {
					_, err := parseFileAttr(s)
					return err
				}))},
			},
			"exclusive": schema.BoolAttribute{
				Optional: true,
				Computed: true,
				Default:  booldefault.StaticBool(defaultFileAttrsExclusive),
				MarkdownDescription: "Whether `attributes` is the complete list of flags. " +
					"With `true`, every supported flag that is not listed is cleared, and flags set by others show up as drift. " +
					"With `false`, only the listed flags are ensured, and other flags, such as those a file inherits from its directory, are left alone. " +
					"Flags that are not supported by this resource, such as `e` (extents), are never changed. Defaults to `false`.",
			},
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Resource identifier (equal to `path`).",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
		},
	}
}

// fileAttrsSpec is the part of a fileAttributesModel that says what to
// apply.
type fileAttrsSpec struct {
	want      uint32
	exclusive bool
}

// managed returns the flags the spec decides: the listed ones, or with
// exclusive, every supported flag.
func (s fileAttrsSpec) managed() uint32 {
	if s.exclusive {
		return fileAttrAllFlags
	}
	return s.want
}

func (m *fileAttributesModel) spec(ctx context.Context) (fileAttrsSpec, diag.Diagnostics) {
	var diags diag.Diagnostics
	s := fileAttrsSpec{exclusive: !m.Exclusive.IsNull() && m.Exclusive.ValueBool()}
	if m.Attributes.IsNull() || m.Attributes.IsUnknown() {
		return s, diags
	}
	var letters []string
	diags.Append(m.Attributes.ElementsAs(ctx, &letters, false)...)
	if diags.HasError() {
		return s, diags
	}
	want, err := parseFileAttrs(letters)
	if err != nil {
		diags.AddAttributeError(path.Root("attributes"), "Invalid inode flag", capitalize(err.Error())+".")
	}
	s.want = want
	return s, diags
}

// fileAttrsOriginal returns the inode flags recorded in private state when
// the resource was created, or ok = false if there are none, as after an
// import.
func fileAttrsOriginal(ctx context.Context, p privateState) (flags uint32, ok bool, diags diag.Diagnostics) {
	raw, diags := p.GetKey(ctx, fileAttrsPrivateOriginal)
	if diags.HasError() || len(raw) == 0 {
		return 0, false, diags
	}
	if err := json.Unmarshal(raw, &flags); err != nil {
		diags.AddError("Invalid private state", fmt.Sprintf("Decoding %q: %s.", fileAttrsPrivateOriginal, err))
		return 0, false, diags
	}
	return flags, true, diags
}

func (r *fileAttributesResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan fileAttributesModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	spec, diags := plan.spec(ctx)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	original, diags := r.apply(plan.Path.ValueString(), nil, spec, nil)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(setPrivateJSON(ctx, resp.Private, fileAttrsPrivateOriginal, original)...)
	plan.ID = plan.Path
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *fileAttributesResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state fileAttributesModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	spec, diags := plan.spec(ctx)
	resp.Diagnostics.Append(diags...)
	prev, diags := state.spec(ctx)
	resp.Diagnostics.Append(diags...)
	original, known, diags := fileAttrsOriginal(ctx, req.Private)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	var orig *uint32
	if known {
		orig = &original
	}
	_, diags = r.apply(plan.Path.ValueString(), &prev, spec, orig)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	plan.ID = plan.Path
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// resolveAttrsPath checks the managed path p, which comes from state on read
// and delete, and resolves it inside root_dir.
func (r *fileAttributesResource) resolveAttrsPath(p string) (string, diag.Diagnostics) {
	var diags diag.Diagnostics
	// Config validation already enforces this; state is not validated.
	if err := validateAbsolutePath(p); err != nil {
		diags.AddAttributeError(path.Root("path"), "Invalid path", capitalize(err.Error())+".")
		return "", diags
	}
	return resolvePathAttr(r.root(), p, false)
}

// fileAttrsDiag returns an error diagnostic on the path attribute for err.
func fileAttrsDiag(summary string, err error) diag.Diagnostic {
	return diag.NewAttributeErrorDiagnostic(path.Root("path"), summary, capitalize(err.Error())+".")
}

// nextFileAttrs returns the flags that applying spec to a file with the
// flags cur gives. Flags that prev managed but spec doesn't are restored to
// their value in original, or cleared if original is nil.
func nextFileAttrs(cur uint32, prev *fileAttrsSpec, spec fileAttrsSpec, original *uint32) uint32 {
	next := cur
	if prev != nil {
		released := prev.managed() &^ spec.managed()
		next &^= released
		if original != nil {
			next |= *original & released
		}
	}
	managed := spec.managed()
	return next&^managed | spec.want&managed
}

// observedFileAttrs returns the attributes to record in state for a file
// with the flags actual: with exclusive, every supported flag that is set,
// and otherwise the listed flags that are set.
func observedFileAttrs(actual uint32, spec fileAttrsSpec) uint32 {
	return actual & spec.managed()
}

// apply sets the inode flags of p as spec says. prev is the previously
// applied spec on update, or nil on create. original holds the flags from
// before the resource was created, if known. It returns the flags p had
// before.
func (r *fileAttributesResource) apply(p string, prev *fileAttrsSpec, spec fileAttrsSpec, original *uint32) (uint32, diag.Diagnostics) {
	var diags diag.Diagnostics
	target, d := r.resolveAttrsPath(p)
	diags.Append(d...)
	if diags.HasError() {
		return 0, diags
	}
	unlock, err := lockFileForEdit(target)
	if err != nil {
		diags.AddAttributeError(path.Root("path"), "Locking file", capitalize(err.Error())+".")
		return 0, diags
	}
	defer unlock()
	f, err := openACLTarget(target)
	if err != nil {
		diags.Append(fileAttrsDiag("Accessing file", err))
		return 0, diags
	}
	defer func() { _ = f.Close() }()

	cur, err := getFileAttrs(f.f)
	if err != nil {
		diags.Append(fileAttrsDiag("Reading inode flags", err))
		return 0, diags
	}
	diags.Append(writeFileAttrs(f, cur, nextFileAttrs(cur, prev, spec, original), spec.managed())...)
	return cur, diags
}

// writeFileAttrs sets the flags of f from cur to next and checks that the
// flags in check took: some file systems silently ignore flags they don't
// support.
func writeFileAttrs(f *aclFile, cur, next, check uint32) diag.Diagnostics {
	var diags diag.Diagnostics
	if next == cur {
		return diags
	}
	if err := setFileAttrs(f.f, next); err != nil {
		detail := capitalize(err.Error()) + "."
		if errors.Is(err, fs.ErrPermission) {
			if (cur^next)&(fsImmutableFL|fsAppendFL) != 0 {
				detail += " Setting or clearing the immutable (i) and append-only (a) flags needs the CAP_LINUX_IMMUTABLE capability, which root has."
			} else {
				detail += " Changing inode flags needs the file's owner or the CAP_FOWNER capability; the j flag needs CAP_SYS_RESOURCE."
			}
		}
		diags.AddAttributeError(path.Root("attributes"), "Setting inode flags", detail)
		return diags
	}
	got, err := getFileAttrs(f.f)
	if err != nil {
		diags.Append(fileAttrsDiag("Reading inode flags", err))
		return diags
	}
	if missing := (got ^ next) & check & fileAttrAllFlags; missing != 0 {
		diags.AddAttributeError(path.Root("attributes"), "Setting inode flags",
			fmt.Sprintf("The file system of %q did not apply the flags %s (lsattr shows %s); it doesn't support them for this file. Remove them from attributes.",
				f.f.Name(), describeFileAttrs(missing), lsattrString(got)))
	}
	return diags
}

// lsattrString returns the letters of the managed flags in flags, such as
// "id", or "none".
func lsattrString(flags uint32) string {
	if s := strings.Join(formatFileAttrs(flags), ""); s != "" {
		return s
	}
	return "none"
}

func (r *fileAttributesResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state fileAttributesModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	// attributes is required, so it is only null right after import, which
	// adopts every flag that is set.
	imported := state.Attributes.IsNull()
	spec, diags := state.spec(ctx)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	if imported {
		spec.exclusive = true
	}

	target, diags := r.resolveAttrsPath(state.Path.ValueString())
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	f, err := openACLTarget(target)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		resp.State.RemoveResource(ctx)
		return
	case err != nil:
		resp.Diagnostics.Append(fileAttrsDiag("Accessing file", err))
		return
	}
	defer func() { _ = f.Close() }()
	actual, err := getFileAttrs(f.f)
	if err != nil {
		resp.Diagnostics.Append(fileAttrsDiag("Reading inode flags", err))
		return
	}
	attrs, diags := types.SetValueFrom(ctx, types.StringType, formatFileAttrs(observedFileAttrs(actual, spec)))
	resp.Diagnostics.Append(diags...)
	state.Attributes = attrs
	if state.Exclusive.IsNull() {
		state.Exclusive = types.BoolValue(defaultFileAttrsExclusive)
	}
	state.ID = state.Path
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *fileAttributesResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state fileAttributesModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	spec, diags := state.spec(ctx)
	resp.Diagnostics.Append(diags...)
	original, known, diags := fileAttrsOriginal(ctx, req.Private)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	target, diags := r.resolveAttrsPath(state.Path.ValueString())
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	unlock, err := lockFileForEdit(target)
	if err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("path"), "Locking file", capitalize(err.Error())+".")
		return
	}
	defer unlock()
	f, err := openACLTarget(target)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return
	case errors.Is(err, errNotACLTarget):
		// A symlink or special file now occupies the path; its flags are
		// not ours to change.
		resp.Diagnostics.AddWarning("File already replaced",
			fmt.Sprintf("%s. Its inode flags were left untouched.", capitalize(err.Error())))
		return
	case err != nil:
		resp.Diagnostics.Append(fileAttrsDiag("Accessing file", err))
		return
	}
	defer func() { _ = f.Close() }()

	cur, err := getFileAttrs(f.f)
	if err != nil {
		resp.Diagnostics.Append(fileAttrsDiag("Reading inode flags", err))
		return
	}
	// The managed flags go back to what they were before create; without a
	// record of that, as after import, they are cleared.
	managed := spec.managed()
	next := cur &^ managed
	if known {
		next |= original & managed
	}
	resp.Diagnostics.Append(writeFileAttrs(f, cur, next, managed)...)
}

func (r *fileAttributesResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	if err := validateAbsolutePath(req.ID); err != nil {
		resp.Diagnostics.AddError("Invalid import ID",
			fmt.Sprintf("Import ID must be the absolute path of the file or directory: %s.", strings.TrimSuffix(err.Error(), ".")))
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("path"), req.ID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), req.ID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("exclusive"), defaultFileAttrsExclusive)...)
}
