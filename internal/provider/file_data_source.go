package provider

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var _ datasource.DataSource = (*fileDataSource)(nil)

func NewFileDataSource() datasource.DataSource { return &fileDataSource{} }

type fileDataSource struct{}

type fileDataSourceModel struct {
	Path           types.String `tfsdk:"path"`
	FollowSymlinks types.Bool   `tfsdk:"follow_symlinks"`
	Content        types.String `tfsdk:"content"`
	ContentBase64  types.String `tfsdk:"content_base64"`
	ContentSHA256  types.String `tfsdk:"content_sha256"`
	ContentMD5     types.String `tfsdk:"content_md5"`
	Size           types.Int64  `tfsdk:"size"`
	Mode           types.String `tfsdk:"mode"`
	Owner          types.String `tfsdk:"owner"`
	Group          types.String `tfsdk:"group"`
	UID            types.Int64  `tfsdk:"uid"`
	GID            types.Int64  `tfsdk:"gid"`
	Modified       types.String `tfsdk:"modified"`
	ID             types.String `tfsdk:"id"`
}

func (d *fileDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_file"
}

func (d *fileDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Reads an existing regular file's contents, checksums, size, mode, ownership and modification time without managing it. " +
			"Use it to feed the contents of a host file into other resources, to react to changes of a file maintained by something else, or to copy a file's ownership.",
		Attributes: map[string]schema.Attribute{
			"path": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "Absolute path of the file to read. " +
					"Must be in canonical form (no `.`/`..` segments, duplicate or trailing slashes) and must not be `/`.",
				Validators: []validator.String{absolutePath()},
			},
			"follow_symlinks": schema.BoolAttribute{
				Optional: true,
				MarkdownDescription: "If `true` and `path` is a symlink, read the file it points to; all attributes then describe the target, not the link. " +
					"Defaults to `false`, in which case a symlink at `path` is an error.",
			},
			"content": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "File contents as text. Null if the file is not valid UTF-8; use `content_base64` for binary files.",
			},
			"content_base64": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "File contents as standard (padded) base64. Always set, also for text files.",
			},
			"content_sha256": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Hex-encoded SHA-256 checksum of the file contents.",
			},
			"content_md5": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "Hex-encoded MD5 checksum of the file contents. " +
					"Provided for interoperability; use `content_sha256` where integrity matters.",
			},
			"size": schema.Int64Attribute{
				Computed:            true,
				MarkdownDescription: "Size of the contents in bytes.",
			},
			"mode": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Four-digit octal mode of the file, including setuid, setgid and sticky bits (e.g. `\"0644\"`, `\"4755\"`).",
			},
			"owner": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Username of the owner, or the numeric UID as a string if it has no passwd entry.",
			},
			"group": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Group name, or the numeric GID as a string if it has no group entry.",
			},
			"uid": schema.Int64Attribute{
				Computed:            true,
				MarkdownDescription: "Numeric UID of the owner.",
			},
			"gid": schema.Int64Attribute{
				Computed:            true,
				MarkdownDescription: "Numeric GID of the group.",
			},
			"modified": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "Last modification time in [RFC 3339](https://www.rfc-editor.org/rfc/rfc3339) format, in UTC with second precision (e.g. " +
					"`\"2024-02-29T11:34:56Z\"`).",
			},
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Data source identifier (equal to `path`).",
			},
		},
	}
}

func (d *fileDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var cfg fileDataSourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}

	target := cfg.Path.ValueString()
	// Config validation already enforces this; re-check as defense in depth.
	if err := validateAbsolutePath(target); err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("path"), "Invalid path", capitalize(err.Error())+".")
		return
	}

	state, diags := inspectFile(target, cfg.FollowSymlinks.ValueBool())
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	state.FollowSymlinks = cfg.FollowSymlinks
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// openForInspection opens target read-only. Unless follow is set, a symlink
// at target is refused (intermediate components are still resolved, as for
// the resources; see safefs.go). The open is non-blocking so that a FIFO
// cannot hang the provider; the caller must check the file type via Stat on
// the returned descriptor.
func openForInspection(target string, follow bool) (*os.File, error) {
	if !follow {
		return openNoFollow(target, os.O_RDONLY, 0)
	}
	return os.OpenFile(target, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
}

// inspectFile builds the data source model for the regular file at target.
// All metadata comes from the same descriptor that the content is read from,
// so it describes exactly the file that was read even if target is replaced
// concurrently.
func inspectFile(target string, follow bool) (fileDataSourceModel, diag.Diagnostics) {
	var diags diag.Diagnostics
	m := fileDataSourceModel{
		Path:    types.StringValue(target),
		ID:      types.StringValue(target),
		Content: types.StringNull(),
	}

	f, err := openForInspection(target, follow)
	if err != nil {
		switch {
		case errors.Is(err, fs.ErrNotExist) && isSymlink(target):
			diags.AddAttributeError(path.Root("path"), "File not found",
				fmt.Sprintf("Path %q is a symbolic link whose target does not exist.", target))
		case errors.Is(err, fs.ErrNotExist):
			diags.AddAttributeError(path.Root("path"), "File not found",
				fmt.Sprintf("No file exists at %q.", target))
		case !follow && isSymlink(target):
			diags.AddAttributeError(path.Root("path"), "Symbolic link refused",
				fmt.Sprintf("Path %q is a symbolic link. Set follow_symlinks = true to read the file it points to.", target))
		default:
			diags.AddAttributeError(path.Root("path"), "Open failed", capitalize(err.Error())+".")
		}
		return m, diags
	}
	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil {
		diags.AddError("Stat failed", err.Error())
		return m, diags
	}
	switch {
	case info.IsDir():
		diags.AddAttributeError(path.Root("path"), "Is a directory",
			fmt.Sprintf("Path %q is a directory, not a file. Use the sysutils_directory data source to read directories.", target))
		return m, diags
	case !info.Mode().IsRegular():
		diags.AddAttributeError(path.Root("path"), "Not a regular file",
			fmt.Sprintf("Path %q exists but is not a regular file (%s).", target, info.Mode().Type()))
		return m, diags
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		diags.AddError("Stat failed", fmt.Sprintf("Unable to determine ownership of %q on this platform.", target))
		return m, diags
	}

	content, err := io.ReadAll(f)
	if err != nil {
		diags.AddError("Read failed", fmt.Sprintf("Reading %q: %s.", target, err))
		return m, diags
	}
	sums, err := checksumReader(bytes.NewReader(content))
	if err != nil {
		diags.AddError("Read failed", err.Error())
		return m, diags
	}

	// Terraform strings must be valid UTF-8.
	if utf8.Valid(content) {
		m.Content = types.StringValue(string(content))
	}
	m.ContentBase64 = types.StringValue(base64.StdEncoding.EncodeToString(content))
	m.ContentSHA256 = types.StringValue(sums.sha256)
	m.ContentMD5 = types.StringValue(sums.md5)
	// Use the number of bytes actually read, so size always matches the
	// checksums even if the file was modified after fstat.
	m.Size = types.Int64Value(int64(len(content)))

	owner, group := ownerAndGroup(st)
	m.Mode = types.StringValue(formatMode(info.Mode()))
	m.Owner = types.StringValue(owner)
	m.Group = types.StringValue(group)
	m.UID = types.Int64Value(int64(st.Uid))
	m.GID = types.Int64Value(int64(st.Gid))
	m.Modified = types.StringValue(info.ModTime().UTC().Format(time.RFC3339))
	return m, diags
}

// isSymlink reports whether p itself (not what it points to) is a symlink.
func isSymlink(p string) bool {
	info, err := os.Lstat(p)
	return err == nil && info.Mode()&fs.ModeSymlink != 0
}
