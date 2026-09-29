package provider

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/hashicorp/terraform-plugin-framework-validators/resourcevalidator"
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
	_ resource.Resource                     = (*fileLineResource)(nil)
	_ resource.ResourceWithImportState      = (*fileLineResource)(nil)
	_ resource.ResourceWithConfigValidators = (*fileLineResource)(nil)
	_ resource.ResourceWithModifyPlan       = (*fileLineResource)(nil)
	_ resource.ResourceWithConfigure        = (*fileLineResource)(nil)
)

const (
	// maxFileLineSize bounds how much of a file sysutils_file_line reads into
	// memory. Files it is meant for (/etc/hosts, sshd_config, ...) are tiny.
	maxFileLineSize = 64 << 20

	// fileLineCreateMode is the mode of a file created because create = true.
	fileLineCreateMode fs.FileMode = 0o644
)

func NewFileLineResource() resource.Resource { return &fileLineResource{} }

type fileLineResource struct{ rootedResource }

type fileLineModel struct {
	Path         types.String `tfsdk:"path"`
	Line         types.String `tfsdk:"line"`
	Block        types.String `tfsdk:"block"`
	Regexp       types.String `tfsdk:"regexp"`
	Marker       types.String `tfsdk:"marker"`
	InsertAfter  types.String `tfsdk:"insert_after"`
	InsertBefore types.String `tfsdk:"insert_before"`
	Create       types.Bool   `tfsdk:"create"`
	ContentSHA   types.String `tfsdk:"content_sha256"`
	ID           types.String `tfsdk:"id"`
}

func (r *fileLineResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_file_line"
}

func (r *fileLineResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages a single `line`, or a `block` of lines between marker comments, inside an existing text file " +
			"without taking over the rest of the file, like Ansible's `lineinfile` and `blockinfile`. " +
			"Changes are written atomically and keep the file's mode and ownership. On destroy, only the managed line or block is removed.",
		Attributes: map[string]schema.Attribute{
			"path": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "Absolute path of the file to edit. " +
					"Must be in canonical form (no `.`/`..` segments, duplicate or trailing slashes) and must not be `/`. " +
					"A symlink at `path` is refused. Changing this forces a new resource.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
				Validators:    []validator.String{absolutePath()},
			},
			"line": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "The exact line (without line break) that must be present in the file. " +
					"Exactly one of `line` and `block` must be set.",
				Validators: []validator.String{singleLine()},
			},
			"block": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "Lines to keep between a `BEGIN` and an `END` marker line (see `marker`). " +
					"A single trailing newline, as produced by heredocs, is ignored. " +
					"Exactly one of `line` and `block` must be set.",
			},
			"regexp": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "Only with `line`. A [Go regular expression](https://pkg.go.dev/regexp/syntax) selecting an existing line to replace. " +
					"If any line matches, the **last** matching line is replaced by `line`; otherwise `line` is inserted as if `regexp` were unset. " +
					"`line` should itself match `regexp`, so that repeated applies converge.",
				Validators: []validator.String{regexpString()},
			},
			"marker": schema.StringAttribute{
				Optional: true,
				Computed: true,
				Default:  stringdefault.StaticString(defaultFileLineMarker),
				MarkdownDescription: "Only with `block`. Template for the marker lines around the block; `{mark}` is replaced with `BEGIN` and `END`. " +
					"Use a distinct marker for every block in the same file. Defaults to `\"" + defaultFileLineMarker + "\"`.",
				Validators: []validator.String{markerTemplate()},
			},
			"insert_after": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "Where to insert the line or block if it is not present: `\"EOF\"` for the end of the file, " +
					"or a Go regular expression to insert after the last matching line (at the end of the file if no line matches). " +
					"Conflicts with `insert_before`. If neither is set, content is appended at the end of the file. " +
					"Only affects where missing content is added; content that is already present is never moved.",
				Validators: []validator.String{insertPosition(insertAtEOF)},
			},
			"insert_before": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "Where to insert the line or block if it is not present: `\"BOF\"` for the beginning of the file, " +
					"or a Go regular expression to insert before the last matching line (at the end of the file if no line matches). " +
					"Conflicts with `insert_after`.",
				Validators: []validator.String{insertPosition(insertAtBOF)},
			},
			"create": schema.BoolAttribute{
				Optional: true,
				Computed: true,
				Default:  booldefault.StaticBool(false),
				MarkdownDescription: "Create the file (mode `0644`, owned by the user running Terraform, missing parent directories with mode `0755`) if it does not exist. " +
					"If `false`, a missing file is an error. Destroy never deletes the file. Defaults to `false`.",
			},
			"content_sha256": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "Hex-encoded SHA-256 checksum of the managed fragment as it appears in the file: the line, or the block including its marker lines, each followed by a line break. " +
					"Computed from the configuration during plan and refreshed from the file, so a change is visible in the plan even when `line` or `block` comes from a sensitive value and Terraform hides it.",
			},
			"id": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "Resource identifier: `path`, a colon, and then `marker` for blocks or the word `line` for lines. " +
					"The content of `line` is left out, so that a secret in it, even one from a sensitive variable, is not shown in plans as part of the id.",
			},
		},
	}
}

func (r *fileLineResource) ConfigValidators(_ context.Context) []resource.ConfigValidator {
	return []resource.ConfigValidator{
		resourcevalidator.ExactlyOneOf(path.MatchRoot("line"), path.MatchRoot("block")),
		resourcevalidator.Conflicting(path.MatchRoot("regexp"), path.MatchRoot("block")),
		resourcevalidator.Conflicting(path.MatchRoot("marker"), path.MatchRoot("line")),
		resourcevalidator.Conflicting(path.MatchRoot("insert_after"), path.MatchRoot("insert_before")),
	}
}

// ModifyPlan computes the id and the checksum of the desired fragment, and
// rejects combinations that only make sense
// together, such as a block containing its own marker line, at plan time.
func (r *fileLineResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.Plan.Raw.IsNull() {
		return // Destroy.
	}
	var plan fileLineModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if !plan.allKnown() {
		resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("id"), types.StringUnknown())...)
		resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("content_sha256"), types.StringUnknown())...)
		return
	}
	spec, err := plan.spec()
	if err != nil {
		resp.Diagnostics.AddError("Invalid configuration", capitalize(err.Error())+".")
		return
	}
	resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("id"), plan.id())...)
	resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("content_sha256"), fragmentChecksum(spec.render()))...)
}

func (r *fileLineResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan fileLineModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(r.apply(&plan, nil)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *fileLineResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state fileLineModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	target, diags := resolvePathAttr(r.root(), state.Path.ValueString(), false)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	found, diags := refreshFileLine(target, &state)
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

func (r *fileLineResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state fileLineModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(r.apply(&plan, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *fileLineResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state fileLineModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	// State is not validated by the schema; never act on an invalid path.
	if err := validateAbsolutePath(state.Path.ValueString()); err != nil {
		resp.Diagnostics.AddError("Refusing to edit file", capitalize(err.Error())+".")
		return
	}
	target, diags := resolvePathAttr(r.root(), state.Path.ValueString(), false)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	spec, err := state.spec()
	if err != nil {
		resp.Diagnostics.AddError("Invalid state", capitalize(err.Error())+".")
		return
	}

	unlock := lockFileForEdit(target)
	defer unlock()

	data, snap, err := readRegularFileNoFollow(target, maxFileLineSize)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return // Nothing left to remove.
	case err != nil && isRefusedFileType(target):
		// The file was replaced by a symlink or something else after it was
		// last refreshed. Leave the replacement and whatever it points to
		// untouched.
		resp.Diagnostics.AddWarning("Managed content not removed",
			fmt.Sprintf("%s. The managed content was not removed; the resource is removed from state only.", capitalize(err.Error())))
		return
	case err != nil:
		resp.Diagnostics.AddError("Reading file", err.Error())
		return
	}
	text := parseTextFile(data)
	changed, _, err := spec.remove(text)
	if err != nil {
		resp.Diagnostics.AddError("Removing managed content", fmt.Sprintf("%s in %q.", capitalize(err.Error()), target))
		return
	}
	if !changed {
		return
	}
	if err := replaceFileAtomic(target, text.bytes(), snap, fileLineCreateMode); err != nil {
		resp.Diagnostics.AddError("Writing file", err.Error())
	}
}

// ImportState accepts "<path>:<marker>" for blocks, recognised by the
// "{mark}" placeholder, and "<path>:<line>" for lines. The path ends at the
// first colon, so paths containing colons cannot be imported.
func (r *fileLineResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	target, rest, ok := strings.Cut(req.ID, ":")
	if !ok || rest == "" {
		resp.Diagnostics.AddError("Invalid import ID",
			fmt.Sprintf("Import ID must have the form <path>:<marker> for a block or <path>:<line> for a line, got %q.", req.ID))
		return
	}
	if err := validateAbsolutePath(target); err != nil {
		resp.Diagnostics.AddError("Invalid import ID", fmt.Sprintf("Invalid path in import ID: %s.", err))
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("path"), target)...)
	m := fileLineModel{Path: types.StringValue(target), Line: types.StringNull(), Marker: types.StringValue(rest)}
	if strings.Contains(rest, markPlaceholder) {
		if err := validateMarker(rest); err != nil {
			resp.Diagnostics.AddError("Invalid import ID", capitalize(err.Error())+".")
			return
		}
		resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("marker"), rest)...)
	} else {
		resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("line"), rest)...)
		m.Line = types.StringValue(rest)
	}
	// Not req.ID, which contains the line for lines; see fileLineModel.id.
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), m.id())...)
}

// apply makes the fragment described by plan present in the file. prev is
// the prior state on update and nil on create. If prev addresses a different
// fragment (another line, another marker, or line vs. block) the old one is
// removed and the new one takes its place. On success plan is refreshed from
// the file.
func (r *fileLineResource) apply(plan, prev *fileLineModel) diag.Diagnostics {
	var diags diag.Diagnostics
	// Config validation already enforces this; re-check as defense in depth.
	if err := validateAbsolutePath(plan.Path.ValueString()); err != nil {
		diags.AddAttributeError(path.Root("path"), "Invalid path", capitalize(err.Error())+".")
		return diags
	}
	target, diags := resolvePathAttr(r.root(), plan.Path.ValueString(), false)
	if diags.HasError() {
		return diags
	}
	desired, err := plan.spec()
	if err != nil {
		diags.AddError("Invalid configuration", capitalize(err.Error())+".")
		return diags
	}
	var old *fragmentSpec
	if prev != nil {
		if old, err = prev.spec(); err != nil {
			diags.AddError("Invalid state", capitalize(err.Error())+".")
			return diags
		}
	}

	unlock := lockFileForEdit(target)
	defer unlock()

	data, snap, err := readRegularFileNoFollow(target, maxFileLineSize)
	if errors.Is(err, fs.ErrNotExist) {
		if !plan.Create.ValueBool() {
			diags.AddAttributeError(path.Root("path"), "File does not exist",
				fmt.Sprintf("File %q does not exist. Set create = true to create it.", target))
			return diags
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			diags.AddError("Creating parent directory", err.Error())
			return diags
		}
		data, snap, err = nil, nil, nil
	}
	if err != nil {
		diags.AddAttributeError(path.Root("path"), "Reading file", capitalize(err.Error())+".")
		return diags
	}

	text := parseTextFile(data)
	changed, hint := false, -1
	if old != nil && !old.sameIdentity(desired) {
		if changed, hint, err = old.remove(text); err != nil {
			diags.AddError("Removing previous content", fmt.Sprintf("%s in %q.", capitalize(err.Error()), target))
			return diags
		}
	}
	added, err := desired.ensure(text, hint)
	if err != nil {
		diags.AddError("Editing file", fmt.Sprintf("%s in %q.", capitalize(err.Error()), target))
		return diags
	}
	if changed || added || snap == nil {
		if err := replaceFileAtomic(target, text.bytes(), snap, fileLineCreateMode); err != nil {
			diags.AddError("Writing file", err.Error())
			return diags
		}
	}

	found, refreshDiags := refreshFileLine(target, plan)
	diags.Append(refreshDiags...)
	if !diags.HasError() && !found {
		diags.AddError("Writing file", fmt.Sprintf("The managed content is missing from %q immediately after writing it.", target))
	}
	return diags
}

// refreshFileLine updates m from the file at the host path target. It
// reports found=false if
// the file or the managed fragment is missing, so the resource is planned
// for creation. If a line selected by regexp, or the content between the
// block markers, differs from m, m is updated with the actual content so the
// plan shows a readable diff.
func refreshFileLine(target string, m *fileLineModel) (found bool, diags diag.Diagnostics) {
	spec, err := m.spec()
	if err != nil {
		diags.AddError("Invalid state", capitalize(err.Error())+".")
		return false, diags
	}
	data, _, err := readRegularFileNoFollow(target, maxFileLineSize)
	if errors.Is(err, fs.ErrNotExist) {
		return false, diags
	}
	if err != nil {
		diags.AddAttributeError(path.Root("path"), "Reading file", capitalize(err.Error())+".")
		return false, diags
	}
	lines := parseTextFile(data).lines
	status, start, end, actual, err := spec.inspect(lines)
	if err != nil {
		diags.AddError("Reading managed content", fmt.Sprintf("%s in %q. Remove or complete the block manually.", capitalize(err.Error()), target))
		return false, diags
	}
	switch {
	case status == fragmentAbsent:
		return false, diags
	case spec.isBlock:
		// After import the block is unknown; heredocs end in a newline.
		trailing := m.Block.IsNull() || strings.HasSuffix(m.Block.ValueString(), "\n")
		if status == fragmentDiffers || m.Block.IsNull() {
			m.Block = types.StringValue(strings.ToValidUTF8(joinBlock(actual, trailing), "�"))
		}
	case status == fragmentDiffers:
		m.Line = types.StringValue(strings.ToValidUTF8(actual[0], "�"))
	}

	if m.Marker.IsNull() || m.Marker.IsUnknown() {
		m.Marker = types.StringValue(defaultFileLineMarker)
	}
	if m.Create.IsNull() || m.Create.IsUnknown() {
		m.Create = types.BoolValue(false)
	}
	m.ContentSHA = fragmentChecksum(lines[start:end])
	m.ID = m.id()
	return true, diags
}

// fragmentChecksum returns the hex-encoded SHA-256 of lines, each terminated
// by a line break, as the value of content_sha256.
func fragmentChecksum(lines []string) types.String {
	h := sha256.New()
	for _, l := range lines {
		_, _ = h.Write([]byte(l + "\n")) // hash.Hash writes never fail.
	}
	return types.StringValue(hex.EncodeToString(h.Sum(nil)))
}

// isBlock reports whether m manages a block. After import of a block only
// the marker is known, so anything without a line is a block.
func (m *fileLineModel) isBlock() bool {
	return m.Line.IsNull()
}

func (m *fileLineModel) spec() (*fragmentSpec, error) {
	return newFragmentSpec(fragmentConfig{
		isBlock:      m.isBlock(),
		line:         m.Line.ValueString(),
		block:        m.Block.ValueString(),
		marker:       m.Marker.ValueString(),
		regexp:       m.Regexp.ValueString(),
		insertAfter:  m.InsertAfter.ValueString(),
		insertBefore: m.InsertBefore.ValueString(),
	})
}

// fileLineIDSuffix ends the id of a resource managing a line.
const fileLineIDSuffix = "line"

// id returns the identifier of m: the path and the marker for blocks, and
// the path and the constant fileLineIDSuffix for lines. The line itself is
// deliberately left out. It may contain a secret, such as a password in a
// configuration file, and Terraform does not carry the sensitivity of line
// over to the computed id, so the id would print the secret in plans.
func (m *fileLineModel) id() types.String {
	key := fileLineIDSuffix
	if m.isBlock() {
		key = m.Marker.ValueString()
		if key == "" {
			key = defaultFileLineMarker
		}
	}
	return types.StringValue(m.Path.ValueString() + ":" + key)
}

func (m *fileLineModel) allKnown() bool {
	for _, v := range []types.String{m.Path, m.Line, m.Block, m.Regexp, m.Marker, m.InsertAfter, m.InsertBefore} {
		if v.IsUnknown() {
			return false
		}
	}
	return true
}

// isRefusedFileType reports whether p exists but is not a regular file, as
// opposed to being unreadable for another reason.
func isRefusedFileType(p string) bool {
	info, err := os.Lstat(p)
	return err == nil && !info.Mode().IsRegular()
}

// fileEditLocks serialises read-modify-write cycles on the same path within
// this provider process. Terraform applies independent resources in
// parallel, and several sysutils_file_line resources commonly edit the same
// file; without the lock their atomic replacements would overwrite each
// other's changes.
var fileEditLocks sync.Map // map[string]*sync.Mutex

// lockFileForEdit locks p for editing and returns the unlock function.
func lockFileForEdit(p string) (unlock func()) {
	v, _ := fileEditLocks.LoadOrStore(p, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// singleLineValidator rejects strings containing line breaks.
type singleLineValidator struct{}

func singleLine() validator.String { return singleLineValidator{} }

func (v singleLineValidator) Description(_ context.Context) string {
	return "value must not contain line breaks"
}

func (v singleLineValidator) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}

func (v singleLineValidator) ValidateString(_ context.Context, req validator.StringRequest, resp *validator.StringResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	if err := validateSingleLine(req.ConfigValue.ValueString()); err != nil {
		resp.Diagnostics.AddAttributeError(req.Path, "Invalid line", capitalize(err.Error())+".")
	}
}

// regexpValidator rejects strings that are not valid Go regular expressions.
type regexpValidator struct{}

func regexpString() validator.String { return regexpValidator{} }

func (v regexpValidator) Description(_ context.Context) string {
	return "value must be a valid Go regular expression"
}

func (v regexpValidator) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}

func (v regexpValidator) ValidateString(_ context.Context, req validator.StringRequest, resp *validator.StringResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	if _, err := regexp.Compile(req.ConfigValue.ValueString()); err != nil {
		resp.Diagnostics.AddAttributeError(req.Path, "Invalid regular expression", capitalize(err.Error())+".")
	}
}

// markerValidator requires the "{mark}" placeholder and a single line.
type markerValidator struct{}

func markerTemplate() validator.String { return markerValidator{} }

func (v markerValidator) Description(_ context.Context) string {
	return "value must be a single line containing \"{mark}\""
}

func (v markerValidator) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}

func (v markerValidator) ValidateString(_ context.Context, req validator.StringRequest, resp *validator.StringResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	if err := validateMarker(req.ConfigValue.ValueString()); err != nil {
		resp.Diagnostics.AddAttributeError(req.Path, "Invalid marker", capitalize(err.Error())+".")
	}
}

// insertPositionValidator accepts a keyword (EOF or BOF) or a regular
// expression.
type insertPositionValidator struct{ keyword string }

func insertPosition(keyword string) validator.String {
	return insertPositionValidator{keyword: keyword}
}

func (v insertPositionValidator) Description(_ context.Context) string {
	return fmt.Sprintf("value must be %q or a valid Go regular expression", v.keyword)
}

func (v insertPositionValidator) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}

func (v insertPositionValidator) ValidateString(_ context.Context, req validator.StringRequest, resp *validator.StringResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	if _, err := compileInsertPosition(req.ConfigValue.ValueString(), v.keyword); err != nil {
		resp.Diagnostics.AddAttributeError(req.Path, "Invalid insert position", capitalize(err.Error())+".")
	}
}
