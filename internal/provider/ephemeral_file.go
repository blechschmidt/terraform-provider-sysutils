package provider

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"unicode/utf8"

	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework/ephemeral"
	"github.com/hashicorp/terraform-plugin-framework/ephemeral/schema"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

const (
	// defaultEphemeralFileMaxSize is the default of max_size of the
	// sysutils_file ephemeral resource.
	defaultEphemeralFileMaxSize = 1 << 20
	// maxEphemeralFileSize is the largest max_size allowed. The contents are
	// held in memory, and passed to Terraform twice (as text and as base64).
	maxEphemeralFileSize = 16 << 20
)

var (
	_ ephemeral.EphemeralResource              = (*fileEphemeralResource)(nil)
	_ ephemeral.EphemeralResourceWithConfigure = (*fileEphemeralResource)(nil)
)

func NewFileEphemeralResource() ephemeral.EphemeralResource { return &fileEphemeralResource{} }

type fileEphemeralResource struct {
	fsRoot *fsRoot
}

type fileEphemeralModel struct {
	Path          types.String `tfsdk:"path"`
	MaxSize       types.Int64  `tfsdk:"max_size"`
	Content       types.String `tfsdk:"content"`
	ContentBase64 types.String `tfsdk:"content_base64"`
	SHA256        types.String `tfsdk:"sha256"`
}

func (e *fileEphemeralResource) Metadata(_ context.Context, req ephemeral.MetadataRequest, resp *ephemeral.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_file"
}

func (e *fileEphemeralResource) Configure(_ context.Context, req ephemeral.ConfigureRequest, resp *ephemeral.ConfigureResponse) {
	data, diags := providerDataFrom(req.ProviderData)
	resp.Diagnostics.Append(diags...)
	if data != nil {
		e.fsRoot = data.root
	}
}

func (e *fileEphemeralResource) root() *fsRoot {
	if e.fsRoot == nil {
		return hostRoot
	}
	return e.fsRoot
}

func (e *fileEphemeralResource) Schema(_ context.Context, _ ephemeral.SchemaRequest, resp *ephemeral.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Reads a local file, typically a secret, without storing it in the plan or the state. " +
			"Pass its contents to write-only arguments such as `content_wo` of the `sysutils_file` resource, to provider configuration, or to other ephemeral contexts. " +
			"The file is read whenever Terraform opens the ephemeral resource, which is during every plan and apply that needs it. " +
			"Only a regular file that nobody but root or the provider's user can change is read: a symlink at `path` is refused, the file must belong to root or the provider's user and must not be writable by its group or others, " +
			"and so must every directory on the way to it (sticky directories such as `/tmp` excepted).",
		Attributes: map[string]schema.Attribute{
			"path": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "Absolute path of the file to read. " +
					"Must be in canonical form (no `.`/`..` segments, duplicate or trailing slashes) and must not be `/`. " +
					"If the provider's `root_dir` is set, the path is inside it.",
				Validators: []validator.String{absolutePath()},
			},
			"max_size": schema.Int64Attribute{
				Optional: true,
				MarkdownDescription: fmt.Sprintf("Largest file size in bytes to read; a larger file is an error. Defaults to `%d` (1 MiB); at most `%d` (16 MiB).",
					defaultEphemeralFileMaxSize, maxEphemeralFileSize),
				Validators: []validator.Int64{int64validator.Between(0, maxEphemeralFileSize)},
			},
			"content": schema.StringAttribute{
				Computed:            true,
				Sensitive:           true,
				MarkdownDescription: "File contents as text. Null if the file is not valid UTF-8; use `content_base64` for binary files.",
			},
			"content_base64": schema.StringAttribute{
				Computed:            true,
				Sensitive:           true,
				MarkdownDescription: "File contents as standard (padded) base64. Always set, also for text files.",
			},
			"sha256": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Hex-encoded SHA-256 checksum of the file contents.",
			},
		},
	}
}

func (e *fileEphemeralResource) Open(ctx context.Context, req ephemeral.OpenRequest, resp *ephemeral.OpenResponse) {
	var m fileEphemeralModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	p := m.Path.ValueString()
	// Config validation already enforces this; re-check as defense in depth.
	if err := validateAbsolutePath(p); err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("path"), "Invalid path", capitalize(err.Error())+".")
		return
	}
	limit := int64(defaultEphemeralFileMaxSize)
	if !m.MaxSize.IsNull() {
		limit = m.MaxSize.ValueInt64()
	}
	if limit < 0 || limit > maxEphemeralFileSize {
		resp.Diagnostics.AddAttributeError(path.Root("max_size"), "Invalid max_size",
			fmt.Sprintf("max_size must be between 0 and %d.", maxEphemeralFileSize))
		return
	}
	host, diags := resolvePathAttr(e.root(), p, false)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	data, err := readTrustedFile(host, limit)
	if err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("path"), "Unable to read file", capitalize(err.Error())+".")
		return
	}

	sum := sha256.Sum256(data)
	m.Content = types.StringNull()
	// Terraform strings must be valid UTF-8.
	if utf8.Valid(data) {
		m.Content = types.StringValue(string(data))
	}
	m.ContentBase64 = types.StringValue(base64.StdEncoding.EncodeToString(data))
	m.SHA256 = types.StringValue(hex.EncodeToString(sum[:]))
	resp.Diagnostics.Append(resp.Result.Set(ctx, &m)...)
}

// readTrustedFile reads the regular file at the host path p, of at most
// limit bytes, if nobody but root or the provider's user can change it or
// what p resolves to: it is for secrets, and a file that another user can
// write, or swap for one of their own, could feed Terraform a value of that
// user's choosing. See checkTrustedDir for the directories on the way; a
// symlink at p itself is refused, and the file must belong to root or the
// provider's user and not be writable by its group or others.
func readTrustedFile(p string, limit int64) ([]byte, error) {
	if _, err := checkTrustedDir(filepath.Dir(p), "secret file"); err != nil {
		return nil, err
	}
	f, err := openNoFollow(p, os.O_RDONLY, 0)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if err := checkRegularFile(p, info); err != nil {
		return nil, err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return nil, fmt.Errorf("unable to determine ownership of %q on this platform", p)
	}
	if euid := uint32(os.Geteuid()); st.Uid != 0 && st.Uid != euid { //nolint:gosec // UIDs fit in 32 bits.
		return nil, fmt.Errorf("file %q belongs to user %d; secret files must belong to root or the provider's user", p, st.Uid)
	}
	if info.Mode()&0o022 != 0 {
		return nil, fmt.Errorf("file %q (mode %s) is writable by its group or other users; secret files must be writable only by their owner (chmod go-w)",
			p, formatMode(info.Mode()))
	}
	if info.Size() > limit {
		return nil, fmt.Errorf("file %q is larger than max_size (%d bytes)", p, limit)
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, fmt.Errorf("reading %q: %w", p, err)
	}
	// The file may have grown since fstat.
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("file %q is larger than max_size (%d bytes)", p, limit)
	}
	return data, nil
}
