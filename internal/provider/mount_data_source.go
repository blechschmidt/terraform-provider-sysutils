package provider

import (
	"context"
	"errors"
	"io/fs"
	"path/filepath"
	"slices"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework-validators/listvalidator"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var (
	_ datasource.DataSource              = (*mountDataSource)(nil)
	_ datasource.DataSourceWithConfigure = (*mountDataSource)(nil)
)

func NewMountDataSource() datasource.DataSource { return &mountDataSource{} }

type mountDataSource struct {
	rootedDataSource
	cfg *mountConfig
}

type mountDataSourceModel struct {
	Path         types.String `tfsdk:"path"`
	FSTypes      types.List   `tfsdk:"fstypes"`
	Fstab        types.Bool   `tfsdk:"fstab"`
	Mounted      types.Bool   `tfsdk:"mounted"`
	Source       types.String `tfsdk:"source"`
	FSType       types.String `tfsdk:"fstype"`
	Options      types.List   `tfsdk:"options"`
	SuperOptions types.List   `tfsdk:"super_options"`
	ReadOnly     types.Bool   `tfsdk:"read_only"`
	InFstab      types.Bool   `tfsdk:"in_fstab"`
	FstabDevice  types.String `tfsdk:"fstab_device"`
	FstabFSType  types.String `tfsdk:"fstab_fstype"`
	FstabOptions types.List   `tfsdk:"fstab_options"`
	Mounts       types.List   `tfsdk:"mounts"`
	ID           types.String `tfsdk:"id"`
}

// mountListEntryModel is one element of mounts.
type mountListEntryModel struct {
	Path         types.String `tfsdk:"path"`
	Source       types.String `tfsdk:"source"`
	FSType       types.String `tfsdk:"fstype"`
	Options      types.List   `tfsdk:"options"`
	SuperOptions types.List   `tfsdk:"super_options"`
	ReadOnly     types.Bool   `tfsdk:"read_only"`
	InFstab      types.Bool   `tfsdk:"in_fstab"`
}

var mountListEntryType = types.ObjectType{AttrTypes: map[string]attr.Type{
	"path":          types.StringType,
	"source":        types.StringType,
	"fstype":        types.StringType,
	"options":       types.ListType{ElemType: types.StringType},
	"super_options": types.ListType{ElemType: types.StringType},
	"read_only":     types.BoolType,
	"in_fstab":      types.BoolType,
}}

func (d *mountDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_mount"
}

// pathModeNote and listModeNote are appended to the descriptions of the
// attributes of one mode.
const (
	pathModeNote = " Null unless `path` is set."
	listModeNote = " Null if `path` is set."
	fstabNote    = " Null if `fstab` is false."
)

func (d *mountDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Reads the mount table of the provider's mount namespace from `/proc/self/mountinfo` and, optionally, `/etc/fstab`. " +
			"With `path`, it describes the file system mounted at one mount point and the `/etc/fstab` entry for it, as `sysutils_mount` reads them; without `path`, it lists every mount in `mounts`, optionally only those of some file system types. " +
			"Nothing on the host changes and no root privileges are needed. " +
			"With the provider's `root_dir` set, the data source sees what a process chrooted there would: `/etc/fstab` is read below `root_dir`, only mounts at or below `root_dir` are reported, and their paths are relative to it.",
		Attributes: map[string]schema.Attribute{
			"path": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "Absolute path of the mount point to look up, in canonical form (no `.`/`..` segments, duplicate or trailing slashes); `\"/\"` is allowed. " +
					"It is compared with the mount points as the kernel reports them, which have all symlinks resolved, so symlinks in `path` are not followed. " +
					"If several file systems are stacked on the mount point, the topmost, visible one is described. Omit it to list all mounts in `mounts`.",
				Validators: []validator.String{absolutePathOrRoot()},
			},
			"fstypes": schema.ListAttribute{
				ElementType: types.StringType,
				Optional:    true,
				MarkdownDescription: "Without `path`: only list mounts of these file system types, as the kernel reports them, such as `[\"ext4\", \"xfs\"]` or `[\"nfs\", \"nfs4\"]`. " +
					"Lists all mounts if omitted. Cannot be combined with `path`.",
				Validators: []validator.List{
					listvalidator.SizeAtLeast(1),
					listvalidator.ValueStringsAre(stringCheck("file system type", validateFSType)),
					listvalidator.ConflictsWith(path.MatchRoot("path")),
				},
			},
			"fstab": schema.BoolAttribute{
				Optional: true,
				MarkdownDescription: "Whether to also read `/etc/fstab`, for `in_fstab` and the `fstab_*` attributes. " +
					"A missing `/etc/fstab` counts as empty. Defaults to `true`.",
			},
			"mounted": schema.BoolAttribute{
				Computed:            true,
				MarkdownDescription: "Whether a file system is mounted at `path`." + pathModeNote,
			},
			"source": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "What is mounted at `path`, as the kernel reports it: a device such as `\"/dev/sda1\"` (tags such as `UUID=` are resolved), a share such as `\"server:/export\"`, or the name given to a virtual file system, such as `\"tmpfs\"`. " +
					"For bind mounts, the device of the file system the bound directory is on." + pathModeNote + " Null if nothing is mounted.",
			},
			"fstype": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "File system type of the mount at `path`, as the kernel reports it, such as `\"ext4\"` or `\"nfs4\"`." + pathModeNote + " Null if nothing is mounted.",
			},
			"options": schema.ListAttribute{
				ElementType:         types.StringType,
				Computed:            true,
				MarkdownDescription: "Per-mount options of the mount at `path`, such as `[\"rw\", \"nosuid\", \"relatime\"]`." + pathModeNote + " Null if nothing is mounted.",
			},
			"super_options": schema.ListAttribute{
				ElementType:         types.StringType,
				Computed:            true,
				MarkdownDescription: "Options of the file system (superblock) mounted at `path`, such as `[\"rw\", \"size=1024k\"]`." + pathModeNote + " Null if nothing is mounted.",
			},
			"read_only": schema.BoolAttribute{
				Computed:            true,
				MarkdownDescription: "Whether the mount at `path` is read-only, on its own or because its file system is." + pathModeNote + " Null if nothing is mounted.",
			},
			"in_fstab": schema.BoolAttribute{
				Computed:            true,
				MarkdownDescription: "Whether `/etc/fstab` has an entry for `path`. A trailing slash on the entry's mount point is ignored." + pathModeNote + fstabNote,
			},
			"fstab_device": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "First field of the `/etc/fstab` entry for `path`, as written, such as `\"UUID=...\"`. If there are several entries, the first one, which `mount(8)` uses, is described." + pathModeNote + fstabNote + " Null if there is no entry.",
			},
			"fstab_fstype": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Third field of the `/etc/fstab` entry for `path`, such as `\"ext4\"` or `\"auto\"`." + pathModeNote + fstabNote + " Null if there is no entry.",
			},
			"fstab_options": schema.ListAttribute{
				ElementType:         types.StringType,
				Computed:            true,
				MarkdownDescription: "Options of the `/etc/fstab` entry for `path`, one per element, such as `[\"defaults\", \"noatime\"]`; `[\"defaults\"]` if the entry has no options field." + pathModeNote + fstabNote + " Null if there is no entry.",
			},
			"mounts": schema.ListNestedAttribute{
				Computed: true,
				MarkdownDescription: "Without `path`: every mount, of the types in `fstypes` if set, in the order they were mounted, so that of several file systems stacked on one mount point the topmost comes last." +
					listModeNote,
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"path":          schema.StringAttribute{Computed: true, MarkdownDescription: "Mount point."},
						"source":        schema.StringAttribute{Computed: true, MarkdownDescription: "What is mounted, as `source` describes it."},
						"fstype":        schema.StringAttribute{Computed: true, MarkdownDescription: "File system type."},
						"options":       schema.ListAttribute{ElementType: types.StringType, Computed: true, MarkdownDescription: "Per-mount options."},
						"super_options": schema.ListAttribute{ElementType: types.StringType, Computed: true, MarkdownDescription: "File system (superblock) options."},
						"read_only":     schema.BoolAttribute{Computed: true, MarkdownDescription: "Whether the mount is read-only."},
						"in_fstab": schema.BoolAttribute{Computed: true,
							MarkdownDescription: "Whether `/etc/fstab` has an entry for the mount point." + fstabNote},
					},
				},
			},
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Data source identifier: `path`, or `\"*\"` without it.",
			},
		},
	}
}

func (d *mountDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	data, diags := providerDataFrom(req.ProviderData)
	resp.Diagnostics.Append(diags...)
	if data != nil {
		d.cfg = data.mount
		d.fsRoot = data.root
	}
}

func (d *mountDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var m mountDataSourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	root := d.root()
	stringList := types.ListNull(types.StringType)
	m.Mounted, m.ReadOnly, m.InFstab = types.BoolNull(), types.BoolNull(), types.BoolNull()
	m.Source, m.FSType, m.FstabDevice, m.FstabFSType = types.StringNull(), types.StringNull(), types.StringNull(), types.StringNull()
	m.Options, m.SuperOptions, m.FstabOptions = stringList, stringList, stringList
	m.Mounts = types.ListNull(mountListEntryType)

	all, err := d.cfg.mnt().mounts()
	if err != nil {
		resp.Diagnostics.AddError("Reading mount table", capitalize(err.Error())+".")
		return
	}
	entries, err := mountsInRoot(root, all)
	if err != nil {
		resp.Diagnostics.AddError("Unable to use root_dir", capitalize(err.Error())+".")
		return
	}
	var fstab *textFile
	if m.Fstab.IsNull() || m.Fstab.ValueBool() {
		fstab, err = readFstabInRoot(root, d.cfg.fstab())
		if err != nil {
			resp.Diagnostics.AddError("Reading fstab", capitalize(err.Error())+".")
			return
		}
	}

	if !m.Path.IsNull() {
		target := m.Path.ValueString()
		m.ID = types.StringValue(target)
		live := topMount(entries, target)
		m.Mounted = types.BoolValue(live != nil)
		if live != nil {
			m.Source, m.FSType = types.StringValue(live.source), types.StringValue(live.fstype)
			m.Options = stringListValue(live.options)
			m.SuperOptions = stringListValue(live.superOptions)
			m.ReadOnly = types.BoolValue(live.readOnly())
		}
		if fstab != nil {
			e, _ := lookupFstabEntry(fstab, target)
			m.InFstab = types.BoolValue(e != nil)
			if e != nil {
				m.FstabDevice, m.FstabFSType = types.StringValue(e.device), optionalString(true, e.fstype)
				m.FstabOptions = stringListValue(e.options)
			}
		}
		resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
		return
	}

	m.ID = types.StringValue("*")
	var fstypes []string
	if !m.FSTypes.IsNull() {
		resp.Diagnostics.Append(m.FSTypes.ElementsAs(ctx, &fstypes, false)...)
		if resp.Diagnostics.HasError() {
			return
		}
	}
	inFstabSet := map[string]bool{}
	if fstab != nil {
		for _, l := range fstab.lines {
			if e, ok := parseFstabLine(l); ok {
				inFstabSet[e.mountPoint] = true
				if strings.HasPrefix(e.mountPoint, "/") {
					inFstabSet[filepath.Clean(e.mountPoint)] = true
				}
			}
		}
	}
	list := make([]mountListEntryModel, 0, len(entries))
	for _, e := range entries {
		if len(fstypes) > 0 && !slices.Contains(fstypes, e.fstype) {
			continue
		}
		inFstab := types.BoolNull()
		if fstab != nil {
			inFstab = types.BoolValue(inFstabSet[e.mountPoint])
		}
		list = append(list, mountListEntryModel{
			Path:         types.StringValue(e.mountPoint),
			Source:       types.StringValue(e.source),
			FSType:       types.StringValue(e.fstype),
			Options:      stringListValue(e.options),
			SuperOptions: stringListValue(e.superOptions),
			ReadOnly:     types.BoolValue(e.readOnly()),
			InFstab:      inFstab,
		})
	}
	mounts, diags := types.ListValueFrom(ctx, mountListEntryType, list)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	m.Mounts = mounts
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

// stringListValue converts s to a list value; nil becomes an empty list.
func stringListValue(s []string) types.List {
	elems := make([]attr.Value, len(s))
	for i, v := range s {
		elems[i] = types.StringValue(v)
	}
	return types.ListValueMust(types.StringType, elems)
}

// mountsInRoot returns the mounts at or below root with their mount points
// made relative to it, as a process chrooted there sees them in its
// mountinfo. With the host root, entries are returned unchanged.
func mountsInRoot(root *fsRoot, entries []mountEntry) ([]mountEntry, error) {
	if root.isHost() {
		return entries, nil
	}
	base, err := root.resolve("/")
	if err != nil {
		return nil, err
	}
	var out []mountEntry
	for _, e := range entries {
		rel, ok := strings.CutPrefix(e.mountPoint, base)
		if !ok || (rel != "" && !strings.HasPrefix(rel, "/")) {
			continue // Outside the root, or a sibling such as /srv/rootfs2.
		}
		if rel == "" {
			rel = "/"
		}
		e.mountPoint = filepath.Clean(rel)
		out = append(out, e)
	}
	return out, nil
}

// readFstabInRoot reads the fstab at the managed path p below root,
// following symlinks inside the root. A missing file reads as empty.
func readFstabInRoot(root *fsRoot, p string) (*textFile, error) {
	data, err := readRootedFile(root, p, maxFstabSize)
	if errors.Is(err, fs.ErrNotExist) {
		return &textFile{}, nil
	}
	if err != nil {
		return nil, err
	}
	return parseTextFile(data), nil
}
