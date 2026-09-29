package provider

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"syscall"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var (
	_ datasource.DataSource              = (*directoryDataSource)(nil)
	_ datasource.DataSourceWithConfigure = (*directoryDataSource)(nil)
)

func NewDirectoryDataSource() datasource.DataSource { return &directoryDataSource{} }

type directoryDataSource struct{ rootedDataSource }

type directoryDataSourceModel struct {
	Path    types.String `tfsdk:"path"`
	Exists  types.Bool   `tfsdk:"exists"`
	Mode    types.String `tfsdk:"mode"`
	Owner   types.String `tfsdk:"owner"`
	Group   types.String `tfsdk:"group"`
	UID     types.Int64  `tfsdk:"uid"`
	GID     types.Int64  `tfsdk:"gid"`
	Entries types.List   `tfsdk:"entries"`
	ID      types.String `tfsdk:"id"`
}

func (d *directoryDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_directory"
}

func (d *directoryDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Reads a directory's mode, ownership and immediate entries without managing it. " +
			"Use it to make decisions based on the state of the host — for example to only create something if a directory exists, to copy the ownership of an existing directory, or to generate one resource per file found in a directory.",
		Attributes: map[string]schema.Attribute{
			"path": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "Absolute path of the directory to read. " +
					"Must be in canonical form (no `.`/`..` segments, duplicate or trailing slashes). " +
					"Unlike the resources, `/` is allowed. Symlinks are followed; if the provider's `root_dir` is set, they are resolved inside it, and a link leading above `root_dir` is an error.",
				Validators: []validator.String{absolutePathOrRoot()},
			},
			"exists": schema.BoolAttribute{
				Computed:            true,
				MarkdownDescription: "Whether the directory exists.",
			},
			"mode": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "Four-digit octal mode of the directory, including setuid, setgid and sticky bits (e.g. `\"0755\"`, `\"1777\"`). " +
					"Null if the directory does not exist.",
			},
			"owner": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "Username of the owner, or the numeric UID as a string if it has no passwd entry. " +
					"Null if the directory does not exist.",
			},
			"group": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Group name, or the numeric GID as a string if it has no group entry. Null if the directory does not exist.",
			},
			"uid": schema.Int64Attribute{
				Computed:            true,
				MarkdownDescription: "Numeric UID of the owner. Null if the directory does not exist.",
			},
			"gid": schema.Int64Attribute{
				Computed:            true,
				MarkdownDescription: "Numeric GID of the group. Null if the directory does not exist.",
			},
			"entries": schema.ListAttribute{
				Computed:    true,
				ElementType: types.StringType,
				MarkdownDescription: "Names (not full paths) of the directory's immediate entries — files, subdirectories, symlinks and so on — sorted lexically, excluding `.` and `..`. " +
					"The listing is not recursive and includes hidden entries. Null if the directory does not exist.",
			},
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Data source identifier (equal to `path`).",
			},
		},
	}
}

func (d *directoryDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var cfg directoryDataSourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}

	// Config validation already enforces this; re-check as defense in depth.
	if err := validateCanonicalPath(cfg.Path.ValueString()); err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("path"), "Invalid path", capitalize(err.Error())+".")
		return
	}
	// Symlinks are followed, inside root_dir if it is set.
	target, diags := resolvePathAttr(d.root(), cfg.Path.ValueString(), true)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	state, diags := inspectDirectory(ctx, target)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	state.Path, state.ID = cfg.Path, cfg.Path
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// inspectDirectory builds the data source model for target. A missing target
// yields exists=false with all other computed attributes null.
func inspectDirectory(ctx context.Context, target string) (directoryDataSourceModel, diag.Diagnostics) {
	var diags diag.Diagnostics
	m := directoryDataSourceModel{
		Path:    types.StringValue(target),
		ID:      types.StringValue(target),
		Exists:  types.BoolValue(false),
		Mode:    types.StringNull(),
		Owner:   types.StringNull(),
		Group:   types.StringNull(),
		UID:     types.Int64Null(),
		GID:     types.Int64Null(),
		Entries: types.ListNull(types.StringType),
	}

	info, err := os.Stat(target)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return m, diags
		}
		diags.AddError("Stat failed", err.Error())
		return m, diags
	}
	if !info.IsDir() {
		diags.AddAttributeError(path.Root("path"), "Not a directory",
			fmt.Sprintf("Path %q exists but is not a directory.", target))
		return m, diags
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		diags.AddError("Stat failed", fmt.Sprintf("Unable to determine ownership of %q on this platform.", target))
		return m, diags
	}

	// os.ReadDir returns entries sorted by name, giving a stable list.
	dirEntries, err := os.ReadDir(target)
	if err != nil {
		diags.AddError("Reading directory", err.Error())
		return m, diags
	}
	names := make([]string, len(dirEntries))
	for i, e := range dirEntries {
		names[i] = e.Name()
	}
	entries, listDiags := types.ListValueFrom(ctx, types.StringType, names)
	diags.Append(listDiags...)
	if diags.HasError() {
		return m, diags
	}

	owner, group := ownerAndGroup(st)
	m.Exists = types.BoolValue(true)
	m.Mode = types.StringValue(formatMode(info.Mode()))
	m.Owner = types.StringValue(owner)
	m.Group = types.StringValue(group)
	m.UID = types.Int64Value(int64(st.Uid))
	m.GID = types.Int64Value(int64(st.Gid))
	m.Entries = entries
	return m, diags
}
