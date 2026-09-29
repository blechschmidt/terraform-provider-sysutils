package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"strings"
	"syscall"

	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/boolplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64default"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var (
	_ resource.Resource               = (*archiveExtractResource)(nil)
	_ resource.ResourceWithConfigure  = (*archiveExtractResource)(nil)
	_ resource.ResourceWithModifyPlan = (*archiveExtractResource)(nil)
)

const (
	defaultArchiveMaxSize    = 1 << 30 // 1 GiB
	defaultArchiveMaxEntries = 100000
	maxStripComponents       = 100
)

func NewArchiveExtractResource() resource.Resource { return &archiveExtractResource{} }

type archiveExtractResource struct{ rootedResource }

type archiveExtractModel struct {
	Source             types.String `tfsdk:"source"`
	Destination        types.String `tfsdk:"destination"`
	StripComponents    types.Int64  `tfsdk:"strip_components"`
	Owner              types.String `tfsdk:"owner"`
	Group              types.String `tfsdk:"group"`
	FileMode           types.String `tfsdk:"file_mode"`
	DirectoryMode      types.String `tfsdk:"directory_mode"`
	MaxSize            types.Int64  `tfsdk:"max_size"`
	MaxEntries         types.Int64  `tfsdk:"max_entries"`
	Overwrite          types.Bool   `tfsdk:"overwrite"`
	ArchiveSHA256      types.String `tfsdk:"archive_sha256"`
	Files              types.List   `tfsdk:"files"`
	DestinationCreated types.Bool   `tfsdk:"destination_created"`
	DriftedEntries     types.Int64  `tfsdk:"drifted_entries"`
	ID                 types.String `tfsdk:"id"`
}

func (r *archiveExtractResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_archive_extract"
}

func (r *archiveExtractResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Extracts a local `.tar`, `.tar.gz`, `.tar.xz` or `.zip` archive into a directory, and removes exactly the extracted files on destroy. " +
			"Entries that would land outside the destination (absolute paths, `..`, symlinks that lead out of it, hard links to files outside the archive) and special files are refused, and the number of entries and the size of the contents are capped. " +
			"The archive is extracted into a temporary directory first and moved into place only once all of it has been extracted and checked.",
		Attributes: map[string]schema.Attribute{
			"source": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "Path to the archive on the machine running Terraform. The format is detected from the file's contents, not its name: " +
					"an uncompressed, gzip- or xz-compressed tar archive, or a zip archive. " +
					"Relative paths are resolved against Terraform's working directory; prefer `${path.module}/...`. The provider's `root_dir` does not apply to `source`. " +
					"The archive is read during every plan, so a change to its contents plans a new extraction even if the configuration is unchanged.",
				Validators: []validator.String{stringvalidator.LengthAtLeast(1)},
			},
			"destination": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "Absolute path of the directory to extract into, below the provider's `root_dir` if that is set. " +
					"If it does not exist, it is created (with missing parents) and removed again on destroy if it is empty then. " +
					"If it exists, the archive is merged into it; files that exist already at the paths of the archive's entries are only replaced with `overwrite = true`. " +
					"Must be in canonical form and must not be `/`. Changing this forces a new resource.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
				Validators:    []validator.String{absolutePath()},
			},
			"strip_components": schema.Int64Attribute{
				Optional: true,
				Computed: true,
				Default:  int64default.StaticInt64(0),
				MarkdownDescription: "Number of leading path components to remove from every entry name, like `tar --strip-components`. " +
					"With `1`, `app-1.2.3/bin/app` is extracted to `bin/app`; entries with no more components than this, such as `app-1.2.3/` itself, are skipped. Defaults to `0`.",
				Validators: []validator.Int64{int64validator.Between(0, maxStripComponents)},
			},
			"owner": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "Username or numeric UID that should own every extracted entry. Requires privileges. " +
					"If unset, extracted entries belong to the user running Terraform; the owners recorded in the archive are never used.",
				Validators: []validator.String{stringvalidator.LengthAtLeast(1)},
			},
			"group": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "Group name or numeric GID of every extracted entry. Requires privileges. " +
					"If unset, extracted entries get the primary group of the user running Terraform.",
				Validators: []validator.String{stringvalidator.LengthAtLeast(1)},
			},
			"file_mode": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "Octal mode for every extracted regular file, such as `\"0644\"`. " +
					"If unset, each file gets the permission bits recorded in the archive, without setuid, setgid and sticky bits and without write permission for group and others (as with a umask of `022`).",
				Validators: []validator.String{octalMode()},
			},
			"directory_mode": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "Octal mode for every extracted directory, and for `destination` if the resource creates it, such as `\"0755\"`. " +
					"If unset, each directory gets the permission bits recorded in the archive, without special bits and without write permission for group and others; directories the archive has no entry for, and a created `destination`, get `0755`.",
				Validators: []validator.String{octalMode()},
			},
			"max_size": schema.Int64Attribute{
				Optional: true,
				Computed: true,
				Default:  int64default.StaticInt64(defaultArchiveMaxSize),
				MarkdownDescription: "Maximum total size of the archive's contents in bytes, after decompression. " +
					"A larger archive is refused before anything is extracted, which protects against decompression bombs. Defaults to `1073741824` (1 GiB).",
				Validators: []validator.Int64{int64validator.AtLeast(1)},
			},
			"max_entries": schema.Int64Attribute{
				Optional: true,
				Computed: true,
				Default:  int64default.StaticInt64(defaultArchiveMaxEntries),
				MarkdownDescription: "Maximum number of entries in the archive, including those that `strip_components` skips. " +
					"A larger archive is refused before anything is extracted. Defaults to `100000`.",
				Validators: []validator.Int64{int64validator.AtLeast(1)},
			},
			"overwrite": schema.BoolAttribute{
				Optional: true,
				Computed: true,
				Default:  booldefault.StaticBool(false),
				MarkdownDescription: "Replace files, symlinks and special files that exist in `destination` at the paths of the archive's entries but were not extracted by this resource. " +
					"They then count as extracted and are removed on destroy. Existing directories are always merged into, never replaced. " +
					"When `false`, such a file makes the extraction fail before anything is changed. Defaults to `false`.",
			},
			"archive_sha256": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "Hex-encoded SHA-256 checksum of the archive that was extracted. " +
					"Computed from `source` during plan, so a changed archive shows up as a planned update.",
			},
			"files": schema.ListAttribute{
				Computed:    true,
				ElementType: types.StringType,
				MarkdownDescription: "The manifest: every path the archive extracts, relative to `destination`, in sorted order, directories with a trailing `/`. " +
					"Includes directories that the archive does not list but that its entries are in. " +
					"Destroy removes exactly these paths: files and symlinks unconditionally, directories only if they are empty then.",
			},
			"destination_created": schema.BoolAttribute{
				Computed:            true,
				MarkdownDescription: "Whether the resource created `destination`, and therefore removes it on destroy if it is empty then.",
				PlanModifiers:       []planmodifier.Bool{boolplanmodifier.UseStateForUnknown()},
			},
			"drifted_entries": schema.Int64Attribute{
				Computed: true,
				MarkdownDescription: "Number of entries of the manifest that no longer look as extracted at the last refresh: missing, of another type, or with other contents, symlink target, mode, owner or group. " +
					"It is always `0` after apply; a non-zero value plans a new extraction (see [Drift Detection](#drift-detection)).",
				PlanModifiers: []planmodifier.Int64{zeroAfterApply{}},
			},
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Resource identifier (equal to `destination`).",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
		},
	}
}

// limits returns the archive limits configured in m.
func (m *archiveExtractModel) limits() archiveLimits {
	return archiveLimits{
		stripComponents: int(m.StripComponents.ValueInt64()),
		maxSize:         m.MaxSize.ValueInt64(),
		maxEntries:      m.MaxEntries.ValueInt64(),
	}
}

// spec returns the extraction settings of m. With lenient, an owner or group
// that no longer exists is ignored rather than an error, so that refresh
// still works after the account was deleted.
func (m *archiveExtractModel) spec(lenient bool) (extractSpec, error) {
	s := extractSpec{limits: m.limits(), uid: -1, gid: -1}
	var err error
	if owner := knownString(m.Owner); owner != "" {
		if s.uid, _, err = resolveOwnership(owner, ""); err != nil {
			if !lenient {
				return s, err
			}
			s.uid = -1
		}
	}
	if group := knownString(m.Group); group != "" {
		if _, s.gid, err = resolveOwnership("", group); err != nil {
			if !lenient {
				return s, err
			}
			s.gid = -1
		}
	}
	for _, v := range []struct {
		attr string
		dst  **fs.FileMode
	}{{knownString(m.FileMode), &s.fileMode}, {knownString(m.DirectoryMode), &s.dirMode}} {
		if v.attr == "" {
			continue
		}
		mode, err := parseMode(v.attr)
		if err != nil {
			return s, err
		}
		*v.dst = &mode
	}
	return s, nil
}

// files returns the manifest stored in m.
func (m *archiveExtractModel) files(ctx context.Context) ([]string, diag.Diagnostics) {
	var files []string
	if m.Files.IsNull() || m.Files.IsUnknown() {
		return nil, nil
	}
	diags := m.Files.ElementsAs(ctx, &files, false)
	return files, diags
}

// setFiles stores files as the manifest of m.
func (m *archiveExtractModel) setFiles(ctx context.Context, files []string) diag.Diagnostics {
	if files == nil {
		files = []string{}
	}
	list, diags := types.ListValueFrom(ctx, types.StringType, files)
	m.Files = list
	return diags
}

// privateExistingDirs is the key in the resource's private state of the
// directories of the manifest that existed before the archive was extracted
// into them, as a JSON list of names. Destroy never removes them, even if
// they are empty then. They are not an attribute: which directories exist
// is only known during apply, and an attribute that changes during apply
// would have to be unknown in every plan.
const privateExistingDirs = "existing_dirs"

// readExistingDirs returns the set stored under privateExistingDirs.
func readExistingDirs(ctx context.Context, p interface {
	GetKey(context.Context, string) ([]byte, diag.Diagnostics)
}) (map[string]bool, diag.Diagnostics) {
	data, diags := p.GetKey(ctx, privateExistingDirs)
	if diags.HasError() || len(data) == 0 {
		return nil, diags
	}
	var names []string
	if err := json.Unmarshal(data, &names); err != nil {
		diags.AddError("Invalid private state", fmt.Sprintf("Unable to decode %s: %s.", privateExistingDirs, err))
		return nil, diags
	}
	return manifestNamesFrom(names), diags
}

// writeExistingDirs stores dirs under privateExistingDirs.
func writeExistingDirs(ctx context.Context, p interface {
	SetKey(context.Context, string, []byte) diag.Diagnostics
}, dirs []string) diag.Diagnostics {
	if dirs == nil {
		dirs = []string{}
	}
	slices.Sort(dirs)
	data, err := json.Marshal(slices.Compact(dirs))
	if err != nil {
		var diags diag.Diagnostics
		diags.AddError("Encoding private state", err.Error())
		return diags
	}
	return p.SetKey(ctx, privateExistingDirs, data)
}

// withoutNames returns the entries of files (with trailing slashes on
// directories) whose names are not in skip.
func withoutNames(files []string, skip map[string]bool) []string {
	out := make([]string, 0, len(files))
	for _, f := range files {
		if !skip[strings.TrimSuffix(f, "/")] {
			out = append(out, f)
		}
	}
	return out
}

// ModifyPlan computes archive_sha256 and files from the archive, so that a
// changed archive plans an update, and validates the archive: a malicious or
// oversized archive fails the plan, not the apply.
func (r *archiveExtractResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.Plan.Raw.IsNull() {
		return // Destroy.
	}
	var plan archiveExtractModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	var state *archiveExtractModel
	if !req.State.Raw.IsNull() {
		state = &archiveExtractModel{}
		resp.Diagnostics.Append(req.State.Get(ctx, state)...)
		if resp.Diagnostics.HasError() {
			return
		}
	}

	unknown := func() {
		resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("archive_sha256"), types.StringUnknown())...)
		resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("files"), types.ListUnknown(types.StringType))...)
	}
	if plan.Source.IsUnknown() || plan.StripComponents.IsUnknown() || plan.MaxSize.IsUnknown() || plan.MaxEntries.IsUnknown() {
		unknown()
		return
	}
	f, err := openSource(plan.Source.ValueString())
	if errors.Is(err, fs.ErrNotExist) {
		if state != nil && state.Source.Equal(plan.Source) && state.StripComponents.Equal(plan.StripComponents) {
			// Keep what was extracted from it: removing a downloaded
			// archive after extraction must not plan a change forever.
			resp.Diagnostics.AddAttributeWarning(path.Root("source"), "Archive not found",
				fmt.Sprintf("The archive %q does not exist, so whether it changed since it was extracted cannot be checked, and a change of any other setting cannot be applied.", plan.Source.ValueString()))
			return
		}
		// It may be created by another resource during the same apply.
		unknown()
		return
	}
	if err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("source"), "Unable to read archive", capitalize(err.Error())+".")
		return
	}
	defer func() { _ = f.Close() }()
	sum, err := archiveSHA256(f)
	if err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("source"), "Unable to read archive", capitalize(err.Error())+".")
		return
	}
	m, err := walkArchive(f, plan.limits(), nil)
	if err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("source"), "Invalid archive",
			fmt.Sprintf("Refusing to extract %q: %s.", plan.Source.ValueString(), err))
		return
	}
	plan.ArchiveSHA256 = types.StringValue(sum)
	resp.Diagnostics.Append(plan.setFiles(ctx, m.fileList())...)
	resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("archive_sha256"), plan.ArchiveSHA256)...)
	resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("files"), plan.Files)...)
}

// destinationAttr resolves the destination of m inside the provider's root
// and reports a failure as an error on the destination attribute.
func (r *archiveExtractResource) destinationAttr(m *archiveExtractModel) (string, diag.Diagnostics) {
	var diags diag.Diagnostics
	dest := m.Destination.ValueString()
	// State is not validated by the schema; never act on an invalid path.
	if err := validateAbsolutePath(dest); err != nil {
		diags.AddAttributeError(path.Root("destination"), "Invalid destination", capitalize(err.Error())+".")
		return "", diags
	}
	host, err := r.root().resolve(dest)
	if err != nil {
		diags.AddAttributeError(path.Root("destination"), "Unable to resolve destination", capitalize(err.Error())+".")
		return "", diags
	}
	return host, diags
}

// openArchive opens the source archive of m and checks that it is still the
// one that was planned, if the plan knows which.
func openArchive(m *archiveExtractModel) (*os.File, diag.Diagnostics) {
	var diags diag.Diagnostics
	f, err := openSource(m.Source.ValueString())
	if err != nil {
		diags.AddAttributeError(path.Root("source"), "Unable to read archive", capitalize(err.Error())+".")
		return nil, diags
	}
	sum, err := archiveSHA256(f)
	if err != nil {
		_ = f.Close()
		diags.AddAttributeError(path.Root("source"), "Unable to read archive", capitalize(err.Error())+".")
		return nil, diags
	}
	if planned := m.ArchiveSHA256; !planned.IsUnknown() && !planned.IsNull() && planned.ValueString() != sum {
		_ = f.Close()
		diags.AddAttributeError(path.Root("source"), "Archive changed since plan",
			fmt.Sprintf("The archive %q has SHA-256 checksum %s, but %s when the plan was made. Nothing was extracted; plan and apply again.",
				m.Source.ValueString(), sum, planned.ValueString()))
		return nil, diags
	}
	m.ArchiveSHA256 = types.StringValue(sum)
	return f, diags
}

func (r *archiveExtractResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan archiveExtractModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	dest, diags := r.destinationAttr(&plan)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	spec, err := plan.spec(false)
	if err != nil {
		resp.Diagnostics.AddError("Invalid extraction settings", capitalize(err.Error())+".")
		return
	}
	f, diags := openArchive(&plan)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	defer func() { _ = f.Close() }()

	res, err := installArchive(ctx, f, dest, spec, nil, plan.Overwrite.ValueBool())
	if res.placed {
		resp.Diagnostics.Append(writeExistingDirs(ctx, resp.Private, res.existingDirs)...)
	}
	plan.ID = plan.Destination
	plan.DriftedEntries = types.Int64Value(0)
	plan.DestinationCreated = types.BoolValue(res.created)
	if err != nil {
		if res.placed {
			// Some entries may be in place; record them all so that the
			// tainted resource removes them when it is replaced.
			resp.Diagnostics.Append(plan.setFiles(ctx, res.manifest.fileList())...)
			resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
		}
		resp.Diagnostics.AddAttributeError(path.Root("source"), "Extracting archive", capitalize(err.Error())+".")
		return
	}
	resp.Diagnostics.Append(plan.setFiles(ctx, res.manifest.fileList())...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *archiveExtractResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state archiveExtractModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	dest, diags := r.destinationAttr(&state)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	info, err := os.Lstat(dest)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		resp.State.RemoveResource(ctx)
		return
	case err != nil:
		resp.Diagnostics.AddAttributeError(path.Root("destination"), "Reading destination", err.Error())
		return
	case !info.IsDir():
		resp.Diagnostics.AddAttributeError(path.Root("destination"), "Not a directory",
			fmt.Sprintf("Destination %q is a symbolic link or not a directory; refusing to follow or replace it.", dest))
		return
	}

	spec, err := state.spec(true)
	if err != nil {
		resp.Diagnostics.AddError("Invalid settings in state", capitalize(err.Error())+".")
		return
	}
	files, diags := state.files(ctx)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	// Contents can only be compared with the archive that was extracted.
	// If it is gone or has changed, the plan reports that through
	// archive_sha256, and only the existence of the entries is checked.
	var archive *os.File
	if f, err := openSource(state.Source.ValueString()); err == nil {
		defer func() { _ = f.Close() }()
		if sum, err := archiveSHA256(f); err == nil && sum == state.ArchiveSHA256.ValueString() {
			archive = f
		}
	}
	drift, err := countDrift(ctx, dest, files, archive, spec)
	if err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("destination"), "Inspecting extracted files", capitalize(err.Error())+".")
		return
	}
	state.DriftedEntries = types.Int64Value(drift)
	state.ID = state.Destination
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *archiveExtractResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state archiveExtractModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	plan.ID = plan.Destination
	plan.DriftedEntries = types.Int64Value(0)
	// The planned value is the one in state; see the comment below.
	plan.DestinationCreated = state.DestinationCreated

	// Only a different archive, different settings for the extracted
	// entries or drift require a new extraction. Changing max_size,
	// max_entries, overwrite or the path of an identical archive does not.
	if plan.ArchiveSHA256.Equal(state.ArchiveSHA256) && plan.Files.Equal(state.Files) &&
		plan.StripComponents.Equal(state.StripComponents) &&
		plan.Owner.Equal(state.Owner) && plan.Group.Equal(state.Group) &&
		plan.FileMode.Equal(state.FileMode) && plan.DirectoryMode.Equal(state.DirectoryMode) &&
		state.DriftedEntries.ValueInt64() == 0 {
		resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
		return
	}

	dest, diags := r.destinationAttr(&plan)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	spec, err := plan.spec(false)
	if err != nil {
		resp.Diagnostics.AddError("Invalid extraction settings", capitalize(err.Error())+".")
		return
	}
	oldFiles, diags := state.files(ctx)
	resp.Diagnostics.Append(diags...)
	oldExisting, diags := readExistingDirs(ctx, req.Private)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	f, diags := openArchive(&plan)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	defer func() { _ = f.Close() }()

	owned := manifestNamesFrom(oldFiles)
	res, err := installArchive(ctx, f, dest, spec, owned, plan.Overwrite.ValueBool())
	// Directories stay pre-existing ones if they were before; the others
	// that existed were created by the previous extraction.
	var existing []string
	for d := range oldExisting {
		// Keep them all if the extraction failed half-way.
		if err != nil || res.manifest.entries[d] != nil {
			existing = append(existing, d)
		}
	}
	for _, d := range res.existingDirs {
		if !owned[d] {
			existing = append(existing, d)
		}
	}
	if res.placed {
		resp.Diagnostics.Append(writeExistingDirs(ctx, resp.Private, existing)...)
	}
	if err != nil {
		if res.placed {
			// Both old and new entries may be in place now.
			resp.Diagnostics.Append(plan.setFiles(ctx, joinManifest(oldFiles, res.manifest.fileList()))...)
			resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
		} else {
			resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
		}
		resp.Diagnostics.AddAttributeError(path.Root("source"), "Extracting archive", capitalize(err.Error())+".")
		return
	}
	// If the destination had disappeared since the plan and was created
	// again, destination_created keeps its planned value (from state):
	// Terraform does not accept another one. It then only means that an
	// empty destination may stay behind on destroy.
	newFiles := res.manifest.fileList()
	warnings, err := removeExtracted(dest, withoutNames(staleEntries(oldFiles, res.manifest), oldExisting))
	for _, w := range warnings {
		resp.Diagnostics.AddWarning("Stale file left in place", w)
	}
	if err != nil {
		resp.Diagnostics.Append(plan.setFiles(ctx, joinManifest(oldFiles, newFiles))...)
		resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
		resp.Diagnostics.AddAttributeError(path.Root("destination"), "Removing files of the previous extraction", capitalize(err.Error())+".")
		return
	}
	resp.Diagnostics.Append(plan.setFiles(ctx, newFiles)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *archiveExtractResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state archiveExtractModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	dest, diags := r.destinationAttr(&state)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	files, diags := state.files(ctx)
	resp.Diagnostics.Append(diags...)
	existing, diags := readExistingDirs(ctx, req.Private)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	warnings, err := removeExtracted(dest, withoutNames(files, existing))
	for _, w := range warnings {
		resp.Diagnostics.AddWarning("File left in place", w)
	}
	if err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("destination"), "Removing extracted files", capitalize(err.Error())+".")
		return
	}
	if !state.DestinationCreated.ValueBool() {
		return
	}
	// rmdir never follows symlinks and only removes empty directories.
	err = syscall.Rmdir(dest)
	switch {
	case err == nil, errors.Is(err, fs.ErrNotExist):
	case errors.Is(err, syscall.ENOTEMPTY), errors.Is(err, syscall.EEXIST):
		resp.Diagnostics.AddWarning("Destination left in place",
			fmt.Sprintf("Directory %q contains files that were not extracted from the archive, so it was not removed.", dest))
	case errors.Is(err, syscall.ENOTDIR):
		resp.Diagnostics.AddWarning("Destination left in place",
			fmt.Sprintf("Path %q is no longer a directory and was left untouched.", dest))
	default:
		resp.Diagnostics.AddAttributeError(path.Root("destination"), "Removing destination", (&fs.PathError{Op: "rmdir", Path: dest, Err: err}).Error())
	}
}
