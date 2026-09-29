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

var _ datasource.DataSource = (*directoryDataSource)(nil)

func NewDirectoryDataSource() datasource.DataSource { return &directoryDataSource{} }

type directoryDataSource struct{}

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
		Description: "Reads metadata and the entry names of a directory on the local filesystem. " +
			"A missing directory is not an error: exists is false and all other attributes are null. " +
			"Symlinks are followed; a path that exists but is not a directory is an error.",
		Attributes: map[string]schema.Attribute{
			"path": schema.StringAttribute{
				Required:    true,
				Description: "Absolute path of the directory to read.",
				Validators:  []validator.String{absolutePathOrRoot()},
			},
			"exists": schema.BoolAttribute{
				Computed:    true,
				Description: "Whether the directory exists.",
			},
			"mode": schema.StringAttribute{
				Computed:    true,
				Description: "Four-digit octal mode of the directory, including special bits (e.g. \"0755\", \"1777\").",
			},
			"owner": schema.StringAttribute{
				Computed:    true,
				Description: "Username of the owner, or the numeric UID if it has no passwd entry.",
			},
			"group": schema.StringAttribute{
				Computed:    true,
				Description: "Group name, or the numeric GID if it has no group entry.",
			},
			"uid": schema.Int64Attribute{
				Computed:    true,
				Description: "Numeric UID of the owner.",
			},
			"gid": schema.Int64Attribute{
				Computed:    true,
				Description: "Numeric GID of the group.",
			},
			"entries": schema.ListAttribute{
				Computed:    true,
				ElementType: types.StringType,
				Description: "Names of the directory's immediate entries (files, subdirectories, symlinks, ...), " +
					"sorted lexically and excluding \".\" and \"..\". Not recursive.",
			},
			"id": schema.StringAttribute{
				Computed:    true,
				Description: "Data source identifier (the directory path).",
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

	target := cfg.Path.ValueString()
	// Config validation already enforces this; re-check as defense in depth.
	if err := validateCanonicalPath(target); err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("path"), "Invalid path", capitalize(err.Error())+".")
		return
	}

	state, diags := inspectDirectory(ctx, target)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
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
