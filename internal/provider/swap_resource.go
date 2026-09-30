package provider

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
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
	_ resource.Resource                   = (*swapResource)(nil)
	_ resource.ResourceWithModifyPlan     = (*swapResource)(nil)
	_ resource.ResourceWithConfigure      = (*swapResource)(nil)
	_ resource.ResourceWithImportState    = (*swapResource)(nil)
	_ resource.ResourceWithValidateConfig = (*swapResource)(nil)
)

const (
	mib = 1 << 20
	// maxSwapSizeMiB is the largest swap file size accepted, 8 TiB. The
	// swap header counts pages in 32 bits, which allows 16 TiB with 4 KiB
	// pages.
	maxSwapSizeMiB = 8 << 20
	// swapDevicePrefix marks paths that name existing block devices.
	swapDevicePrefix = "/dev/"
	// swapSizeTolerance is how far the size of an active swap area may be
	// from its header before it counts as drift. The kernel leaves out
	// the header page and may skip pages of badly aligned file extents.
	swapSizeTolerance = mib
)

func NewSwapResource() resource.Resource { return &swapResource{} }

type swapResource struct {
	rootedResource
	cfg *swapConfig
}

type swapModel struct {
	Path     types.String `tfsdk:"path"`
	SizeMiB  types.Int64  `tfsdk:"size_mib"`
	Priority types.Int64  `tfsdk:"priority"`
	Enabled  types.Bool   `tfsdk:"enabled"`
	Persist  types.Bool   `tfsdk:"persist"`
	Force    types.Bool   `tfsdk:"force"`
	UUID     types.String `tfsdk:"uuid"`
	Created  types.Bool   `tfsdk:"created"`
	ID       types.String `tfsdk:"id"`
}

func (r *swapResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_swap"
}

func (r *swapResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages a swap area: a swap file that the provider allocates, or an existing block device such as a partition. " +
			"Formats it with `mkswap(8)`, enables it with `swapon(8)` and, optionally, adds it to `/etc/fstab` so that it is enabled at boot. " +
			"On destroy the swap area is disabled with `swapoff(8)`, its `/etc/fstab` entry is removed and a swap file the provider created is deleted. " +
			"Requires root privileges.",
		Attributes: map[string]schema.Attribute{
			"path": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "Absolute path of the swap file, or of an existing block device if it starts with `/dev/`, such as `\"/dev/sdb2\"` or `\"/dev/disk/by-uuid/...\"`. " +
					"The parent directory of a swap file must exist, and the file itself must not be a symlink. " +
					"Must be in canonical form (no `.`/`..` segments, duplicate or trailing slashes). Changing this forces a new resource.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
				Validators:    []validator.String{absolutePath()},
			},
			"size_mib": schema.Int64Attribute{
				Optional: true,
				MarkdownDescription: "Size of the swap file in MiB, between 1 and 8388608 (8 TiB). Required for swap files, and must not be set for block devices, which are used whole. " +
					"Changing it disables the swap file, replaces it with one of the new size and enables that. " +
					"Reads as `0` if the file is missing or no longer holds a swap area.",
				Validators: []validator.Int64{int64validator.Between(1, maxSwapSizeMiB)},
			},
			"priority": schema.Int64Attribute{
				Optional: true,
				MarkdownDescription: "Priority of the swap area, between 0 and 32767, as in `swapon -p`. Areas with higher priority are used first; areas with the same priority are used in turn. " +
					"Written to `/etc/fstab` as the `pri=` option. If not set, the kernel assigns a negative priority below all others and it is not managed.",
				Validators: []validator.Int64{int64validator.Between(0, maxSwapPriority)},
			},
			"enabled": schema.BoolAttribute{
				Optional: true,
				Computed: true,
				Default:  booldefault.StaticBool(true),
				MarkdownDescription: "Whether the swap area is in use now. If `false`, it is disabled with `swapoff` but kept, and with `persist = true` still enabled at boot. " +
					"Must be `false` with the provider's `root_dir`, which prepares a swap file and its `/etc/fstab` entry inside the tree without enabling it. Defaults to `true`.",
			},
			"persist": schema.BoolAttribute{
				Optional: true,
				Computed: true,
				Default:  booldefault.StaticBool(true),
				MarkdownDescription: "Whether the swap area has an entry in `/etc/fstab` (inside `root_dir`, if set), so that it is enabled at boot. " +
					"If `true`, the entry `<path> none swap sw[,pri=<priority>] 0 0` is added or updated; if `false`, any swap entry for `path` is removed. Defaults to `true`.",
			},
			"force": schema.BoolAttribute{
				Optional: true,
				Computed: true,
				Default:  booldefault.StaticBool(false),
				MarkdownDescription: "Allow overwriting data. Without it, the provider refuses to run `mkswap` on a block device that holds a file system, volume or partition table signature, " +
					"and refuses to format, resize or replace an existing file it did not create. Swap areas that are already formatted are used as they are either way. Defaults to `false`.",
			},
			"uuid": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "UUID of the swap area, as written by `mkswap`; empty if it has none. It changes whenever the provider formats the swap area again.",
			},
			"created": schema.BoolAttribute{
				Computed: true,
				MarkdownDescription: "Whether the provider created the swap file, in which case destroying the resource deletes it. " +
					"`false` for block devices and for swap files that existed before, which are kept on destroy.",
			},
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Resource identifier (equal to `path`).",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
		},
	}
}

func (r *swapResource) Configure(ctx context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.rootedResource.Configure(ctx, req, resp)
	if data, _ := providerDataFrom(req.ProviderData); data != nil {
		r.cfg = data.swap
	}
}

// isSwapDevice reports whether p names a block device rather than a swap
// file.
func isSwapDevice(p string) bool {
	return strings.HasPrefix(p, swapDevicePrefix)
}

func (r *swapResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var p types.String
	var size types.Int64
	resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root("path"), &p)...)
	resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root("size_mib"), &size)...)
	if resp.Diagnostics.HasError() || p.IsNull() || p.IsUnknown() {
		return
	}
	device := isSwapDevice(p.ValueString())
	switch {
	case device && !size.IsNull():
		resp.Diagnostics.AddAttributeError(path.Root("size_mib"), "Invalid configuration",
			fmt.Sprintf("%q is a block device, which is used whole; remove size_mib.", p.ValueString()))
	case !device && size.IsNull():
		resp.Diagnostics.AddAttributeError(path.Root("size_mib"), "Missing size",
			fmt.Sprintf("size_mib is required for the swap file %q.", p.ValueString()))
	}
}

// ModifyPlan refuses what cannot be done inside root_dir, and marks uuid
// and created unknown when the apply may format or create the swap file
// again.
func (r *swapResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.Plan.Raw.IsNull() {
		return
	}
	var plan swapModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if !r.root().isHost() {
		if !plan.Path.IsUnknown() && isSwapDevice(plan.Path.ValueString()) {
			resp.Diagnostics.AddAttributeError(path.Root("path"), "Not supported with root_dir",
				fmt.Sprintf("Block devices belong to the running host, not to the tree below root_dir = %q. Use a swap file instead.", r.root().String()))
		}
		if plan.Enabled.IsUnknown() || plan.Enabled.ValueBool() {
			resp.Diagnostics.AddAttributeError(path.Root("enabled"), "Not supported with root_dir",
				fmt.Sprintf("swapon enables swap on the running host, not in the tree below root_dir = %q. Set enabled = false to prepare the swap file and its /etc/fstab entry only.", r.root().String()))
		}
	}
	if req.State.Raw.IsNull() {
		return
	}
	var state swapModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	reformat := state.UUID.IsNull() || state.UUID.ValueString() == "" ||
		!plan.SizeMiB.Equal(state.SizeMiB) || plan.Force.ValueBool() && !state.Force.ValueBool()
	if reformat {
		plan.UUID = types.StringUnknown()
		plan.Created = types.BoolUnknown()
	} else {
		plan.UUID = state.UUID
		plan.Created = state.Created
	}
	resp.Diagnostics.Append(resp.Plan.Set(ctx, &plan)...)
}

func (r *swapResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan swapModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	p := plan.Path.ValueString()
	host, fstab, diags := r.paths(p)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	// Refuse to take over a swap area that is already active or in fstab:
	// destroying the resource would disable it or remove the entry.
	t, _, err := readFstab(fstab)
	if err != nil {
		resp.Diagnostics.AddError("Reading fstab", capitalize(err.Error())+".")
		return
	}
	if e, _ := lookupSwapFstabEntry(t, p); e != nil {
		resp.Diagnostics.AddAttributeError(path.Root("path"), "Swap area already in fstab",
			fmt.Sprintf("%s already has a swap entry for %q (%s). Import it with \"terraform import\" instead, or remove the entry.", fstab, p, e.String()))
		return
	}
	if r.root().isHost() {
		live, err := r.lookupSwap(host, isSwapDevice(p))
		if err != nil {
			resp.Diagnostics.AddError("Reading active swap areas", capitalize(err.Error())+".")
			return
		}
		if live != nil {
			resp.Diagnostics.AddAttributeError(path.Root("path"), "Swap area already enabled",
				fmt.Sprintf("%q is already in use as swap. Import it with \"terraform import\" instead, or disable it with swapoff.", p))
			return
		}
	}

	plan.ID = types.StringValue(p)
	plan.Created = types.BoolValue(false)
	changed, diags := r.apply(ctx, &plan, nil, host, fstab)
	resp.Diagnostics.Append(diags...)
	// After a partial apply, record the resource so that it is tainted and
	// destroy can clean up what was done.
	if !resp.Diagnostics.HasError() || changed {
		if plan.UUID.IsUnknown() {
			plan.UUID = types.StringValue("")
		}
		resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
	}
}

func (r *swapResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state swapModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(r.refresh(&state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *swapResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state swapModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	p := plan.Path.ValueString()
	host, fstab, diags := r.paths(p)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	plan.ID = types.StringValue(p)
	plan.Created = types.BoolValue(state.Created.ValueBool())
	changed, diags := r.apply(ctx, &plan, &state, host, fstab)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		if changed {
			// Keep track of a file created in the failed apply, so
			// that destroy still deletes it.
			resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("created"), plan.Created)...)
		}
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *swapResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state swapModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	p := state.Path.ValueString()
	// State is not validated by the schema; never act on an invalid path.
	host, fstab, diags := r.paths(p)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	device := isSwapDevice(p)
	if r.root().isHost() {
		live, err := r.lookupSwap(host, device)
		if err != nil {
			resp.Diagnostics.AddError("Reading active swap areas", capitalize(err.Error())+".")
			return
		}
		// Disable first: if that fails, for lack of memory to take in the
		// swapped-out pages, the fstab entry and the file are kept.
		if live != nil {
			if err := r.cfg.mgr().swapoff(ctx, host); err != nil {
				resp.Diagnostics.AddError("Disabling swap", capitalize(err.Error())+".")
				return
			}
		}
	}
	if err := editFstab(fstab, func(t *textFile) bool { return removeSwapFstabEntries(t, p) }); err != nil {
		resp.Diagnostics.AddError("Editing fstab", fmt.Sprintf("Removing the swap entry for %q from %s: %s.", p, fstab, err))
		return
	}
	if device || !state.Created.ValueBool() {
		return
	}
	info, err := os.Lstat(host)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		resp.Diagnostics.AddError("Deleting swap file", capitalize(err.Error())+".")
	case !info.Mode().IsRegular():
		resp.Diagnostics.AddWarning("Swap file not deleted",
			fmt.Sprintf("%q is no longer a regular file, so it was left alone.", p))
	default:
		if err := os.Remove(host); err != nil && !errors.Is(err, fs.ErrNotExist) {
			resp.Diagnostics.AddError("Deleting swap file", capitalize(err.Error())+".")
		}
	}
}

// ImportState imports a swap area by its path. It is never deleted on
// destroy, as the provider did not create it.
func (r *swapResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	if err := validateCanonicalPath(req.ID); err != nil {
		resp.Diagnostics.AddError("Invalid import ID", fmt.Sprintf("Import ID must be the absolute path of the swap file or device: %s.", err))
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("path"), req.ID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), req.ID)...)
}

// paths validates the managed path p and returns the host path of the swap
// file or device and of the fstab.
func (r *swapResource) paths(p string) (host, fstab string, diags diag.Diagnostics) {
	if err := validateCanonicalPath(p); err != nil {
		diags.AddAttributeError(path.Root("path"), "Invalid path", capitalize(err.Error())+".")
		return "", "", diags
	}
	if p == "/" || p == strings.TrimSuffix(swapDevicePrefix, "/") {
		diags.AddAttributeError(path.Root("path"), "Invalid path", fmt.Sprintf("%q cannot be a swap area.", p))
		return "", "", diags
	}
	if isSwapDevice(p) && !r.root().isHost() {
		diags.AddAttributeError(path.Root("path"), "Not supported with root_dir", "Block devices cannot be used with the provider's root_dir.")
		return "", "", diags
	}
	host, d := resolvePathAttr(r.root(), p, false)
	diags.Append(d...)
	if diags.HasError() {
		return "", "", diags
	}
	fstab, err := r.cfg.fstab(r.root())
	if err != nil {
		diags.AddError("Resolving fstab", capitalize(err.Error())+".")
	}
	return host, fstab, diags
}

// swapName returns the name /proc/swaps shows for the swap area at host:
// the path with all symlinks resolved, except that a symlink at the path
// of a swap file itself is not followed, since the provider never uses one.
func swapName(host string, device bool) string {
	if device {
		if resolved, err := filepath.EvalSymlinks(host); err == nil {
			return resolved
		}
		return host
	}
	dir, err := filepath.EvalSymlinks(filepath.Dir(host))
	if err != nil {
		return host
	}
	return filepath.Join(dir, filepath.Base(host))
}

// lookupSwap returns the active swap area at host, or nil.
func (r *swapResource) lookupSwap(host string, device bool) (*swapEntry, error) {
	entries, err := r.cfg.mgr().swaps()
	if err != nil {
		return nil, err
	}
	return findSwap(entries, swapName(host, device)), nil
}

// apply brings the swap area, its activation and its fstab entry in line
// with plan. prev is the prior state on update and nil on create. It sets
// plan.UUID and plan.Created and reports whether the host may have been
// changed, so that a failed create can still be recorded.
func (r *swapResource) apply(ctx context.Context, plan, prev *swapModel, host, fstab string) (changed bool, diags diag.Diagnostics) {
	p := plan.Path.ValueString()
	device := isSwapDevice(p)
	force := plan.Force.ValueBool()
	onHost := r.root().isHost()
	m := r.cfg.mgr()
	if !onHost && plan.Enabled.ValueBool() {
		diags.AddAttributeError(path.Root("enabled"), "Not supported with root_dir", "enabled = true cannot be used with the provider's root_dir.")
		return false, diags
	}

	var live *swapEntry
	if onHost {
		var err error
		if live, err = r.lookupSwap(host, device); err != nil {
			diags.AddError("Reading active swap areas", capitalize(err.Error())+".")
			return false, diags
		}
	}
	// disable turns the swap area off before it is formatted or replaced.
	disable := func() bool {
		if live == nil {
			return true
		}
		if err := m.swapoff(ctx, host); err != nil {
			diags.AddError("Disabling swap", capitalize(err.Error())+".")
			return false
		}
		live, changed = nil, true
		return true
	}

	var hdr *swapHeader
	var err error
	if device {
		hdr, err = r.prepareDevice(ctx, host, p, force, live != nil)
	} else {
		hdr, err = r.prepareFile(ctx, plan, prev, host, live != nil, disable, &changed)
	}
	if err != nil {
		diags.AddAttributeError(path.Root("path"), "Preparing swap area", capitalize(err.Error())+".")
		return changed, diags
	}
	if diags.HasError() {
		return changed, diags
	}
	if hdr == nil {
		diags.AddAttributeError(path.Root("path"), "Preparing swap area", fmt.Sprintf("%q holds no swap area after formatting it.", p))
		return true, diags
	}
	plan.UUID = types.StringValue(hdr.uuid)

	var priority *int64
	if !plan.Priority.IsNull() {
		v := plan.Priority.ValueInt64()
		priority = &v
	}
	// Enable before writing fstab, so that a swap area the kernel refuses
	// fails the apply before it is written to /etc/fstab.
	if plan.Enabled.ValueBool() {
		if live != nil && priority != nil && live.priority != *priority {
			// The priority of an active swap area cannot be changed.
			if !disable() {
				return changed, diags
			}
		}
		if live == nil {
			if err := m.swapon(ctx, host, priority); err != nil {
				diags.AddError("Enabling swap", capitalize(err.Error())+".")
				return changed, diags
			}
			changed = true
		}
	} else if !disable() {
		return changed, diags
	}

	err = editFstab(fstab, func(t *textFile) bool {
		if plan.Persist.ValueBool() {
			return setSwapFstabEntry(t, swapFstabEntry(p, priority))
		}
		return removeSwapFstabEntries(t, p)
	})
	if err != nil {
		diags.AddError("Editing fstab", fmt.Sprintf("Updating the swap entry for %q in %s: %s.", p, fstab, err))
		return changed, diags
	}

	if !onHost {
		return true, diags
	}
	// Verify the result, so that a swapon that reported success without
	// enabling the swap area fails now instead of showing up as a diff.
	if live, err = r.lookupSwap(host, device); err != nil {
		diags.AddError("Reading active swap areas", capitalize(err.Error())+".")
		return true, diags
	}
	switch {
	case plan.Enabled.ValueBool() && live == nil:
		diags.AddError("Enabling swap", fmt.Sprintf("%q is not listed as active swap area after enabling it.", p))
	case plan.Enabled.ValueBool() && priority != nil && live.priority != *priority:
		diags.AddError("Enabling swap", fmt.Sprintf("%q has priority %d after enabling it with priority %d.", p, live.priority, *priority))
	case !plan.Enabled.ValueBool() && live != nil:
		diags.AddError("Disabling swap", fmt.Sprintf("%q is still an active swap area after disabling it.", p))
	}
	return true, diags
}

// prepareDevice makes sure the block device at host holds a swap area. It
// is formatted only if it holds none, and only if it holds no other
// signature either, unless force is set.
func (r *swapResource) prepareDevice(ctx context.Context, host, p string, force, active bool) (*swapHeader, error) {
	info, err := os.Stat(host)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("block device %q does not exist", p)
	}
	if err != nil {
		return nil, err
	}
	if info.Mode()&fs.ModeDevice == 0 || info.Mode()&fs.ModeCharDevice != 0 {
		return nil, fmt.Errorf("%q is not a block device", p)
	}
	resolved := swapName(host, true)
	hdr, err := readSwapHeader(resolved)
	if err != nil || hdr != nil {
		return hdr, err
	}
	if active {
		return nil, fmt.Errorf("%q is an active swap area, but its swap header cannot be read", p)
	}
	if err := checkBlockDeviceUnused(resolved); err != nil {
		return nil, err
	}
	m := r.cfg.mgr()
	sig, err := m.probe(ctx, resolved, true)
	if err != nil {
		return nil, fmt.Errorf("checking %q for existing data: %w", p, err)
	}
	if sig != "" && !force {
		return nil, fmt.Errorf("%q holds a %s signature, and mkswap would destroy its data; set force = true to format it anyway", p, sig)
	}
	if err := m.mkswap(ctx, resolved, force); err != nil {
		return nil, err
	}
	return readSwapHeader(resolved)
}

// prepareFile makes sure the swap file at host exists, has the planned size
// and holds a swap area, creating or replacing it if needed. Files the
// provider did not create are only replaced with force. It sets
// plan.Created when it creates the file.
func (r *swapResource) prepareFile(ctx context.Context, plan, prev *swapModel, host string, active bool, disable func() bool, changed *bool) (*swapHeader, error) {
	p := plan.Path.ValueString()
	want := plan.SizeMiB.ValueInt64() * mib
	m := r.cfg.mgr()
	info, err := os.Lstat(host)
	if errors.Is(err, fs.ErrNotExist) {
		if active && !disable() {
			return nil, nil
		}
		if err := allocateSwapFile(ctx, m, host, want, false); err != nil {
			return nil, err
		}
		*changed = true
		plan.Created = types.BoolValue(true)
		return readSwapHeader(host)
	}
	if err != nil {
		return nil, err
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		return nil, symlinkRefusedError(p)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%q exists and is not a regular file", p)
	}
	hdr, err := readSwapHeader(host)
	if err != nil {
		return nil, err
	}
	if hdr != nil && hdr.bytes == want && info.Size() == want {
		return hdr, secureSwapFile(host, info)
	}

	owned := prev != nil && prev.Created.ValueBool()
	if !owned && !plan.Force.ValueBool() {
		what := "holds no swap area"
		if hdr != nil {
			what = fmt.Sprintf("is a swap area of %s", formatBytes(hdr.bytes))
			if info.Size() != hdr.bytes {
				what += fmt.Sprintf(" in a file of %s", formatBytes(info.Size()))
			}
		} else if sig, perr := m.probe(ctx, host, false); perr == nil && sig != "" {
			what = "holds a " + sig + " signature"
		}
		return nil, fmt.Errorf("%q already exists and %s, not a swap area of %d MiB. "+
			"The provider did not create it, so it will not overwrite it; set force = true to replace it, or remove it", p, what, plan.SizeMiB.ValueInt64())
	}
	if !disable() {
		return nil, nil
	}
	if err := allocateSwapFile(ctx, m, host, want, true); err != nil {
		return nil, err
	}
	*changed = true
	return readSwapHeader(host)
}

// secureSwapFile makes the existing swap file at host readable and writable
// by its owner only, and owned by root when the provider runs as root, as
// swapon(8) recommends.
func secureSwapFile(host string, info fs.FileInfo) error {
	st, ok := info.Sys().(*syscall.Stat_t)
	needChown := ok && os.Geteuid() == 0 && (st.Uid != 0 || st.Gid != 0)
	if info.Mode().Perm() == swapFileMode && info.Mode()&(fs.ModeSetuid|fs.ModeSetgid|fs.ModeSticky) == 0 && !needChown {
		return nil
	}
	f, err := openNoFollow(host, os.O_RDONLY, 0)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	if needChown {
		if err := f.Chown(0, 0); err != nil {
			return fmt.Errorf("setting ownership of %q: %w", host, err)
		}
	}
	if err := f.Chmod(swapFileMode); err != nil {
		return fmt.Errorf("setting mode of %q: %w", host, err)
	}
	return nil
}

// refresh updates m from the swap file or device, /proc/swaps and fstab.
func (r *swapResource) refresh(m *swapModel) diag.Diagnostics {
	p := m.Path.ValueString()
	host, fstab, diags := r.paths(p)
	if diags.HasError() {
		return diags
	}
	imported := m.Enabled.IsNull()
	device := isSwapDevice(p)

	// The swap area itself.
	var hdr *swapHeader
	var size int64 = -1 // Bytes of a regular file, -1 if there is none.
	exists := false
	var info fs.FileInfo
	var err error
	if device {
		info, err = os.Stat(host)
	} else {
		info, err = os.Lstat(host)
	}
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		diags.AddError("Reading swap area", capitalize(err.Error())+".")
		return diags
	default:
		exists = true
		usable := info.Mode().IsRegular() && !device || device && info.Mode()&fs.ModeDevice != 0 && info.Mode()&fs.ModeCharDevice == 0
		if usable {
			if !device {
				size = info.Size()
			}
			if hdr, err = readSwapHeader(swapName(host, device)); err != nil {
				diags.AddError("Reading swap header", capitalize(err.Error())+".")
				return diags
			}
		}
	}

	var live *swapEntry
	if r.root().isHost() {
		if live, err = r.lookupSwap(host, device); err != nil {
			diags.AddError("Reading active swap areas", capitalize(err.Error())+".")
			return diags
		}
	}
	t, _, err := readFstab(fstab)
	if err != nil {
		diags.AddError("Reading fstab", capitalize(err.Error())+".")
		return diags
	}
	entry, count := lookupSwapFstabEntry(t, p)
	if imported && !exists && live == nil && entry == nil {
		diags.AddError("Cannot import swap area",
			fmt.Sprintf("%q does not exist, is not an active swap area and has no swap entry in %s.", p, fstab))
		return diags
	}
	if count > 1 {
		diags.AddWarning("Duplicate fstab entries",
			fmt.Sprintf("%s has %d swap entries for %q. The next apply that changes this resource removes all but the first.", fstab, count, p))
	}

	m.ID = types.StringValue(p)
	m.Enabled = types.BoolValue(live != nil)
	m.Persist = types.BoolValue(entry != nil)
	if hdr != nil {
		m.UUID = types.StringValue(hdr.uuid)
	} else {
		m.UUID = types.StringValue("")
	}
	if imported {
		m.Force = types.BoolValue(false)
		m.Created = types.BoolValue(false)
	}
	if m.Created.IsNull() {
		m.Created = types.BoolValue(false)
	}

	if device {
		m.SizeMiB = types.Int64Null()
	} else {
		var actual int64 // Bytes of swap area; 0 if there is none.
		if hdr != nil {
			actual = hdr.bytes
			if actual == m.SizeMiB.ValueInt64()*mib {
				actual = size
			}
			if live != nil {
				liveBytes := live.sizeKiB<<10 + int64(os.Getpagesize())
				if d := liveBytes - hdr.bytes; d > swapSizeTolerance || d < -swapSizeTolerance {
					actual = liveBytes
				}
			}
		}
		var want *int64
		if !m.SizeMiB.IsNull() {
			v := m.SizeMiB.ValueInt64()
			want = &v
		}
		m.SizeMiB = types.Int64Value(reportedSizeMiB(actual, want))
	}

	// The priority is recorded in fstab and, while active, in the kernel.
	// A difference in either shows up as a diff; a kernel-assigned
	// priority is not reported unless a priority is configured.
	var want *int64
	if !m.Priority.IsNull() {
		v := m.Priority.ValueInt64()
		want = &v
	}
	var fstabPri *int64
	if entry != nil {
		fstabPri = fstabSwapPriority(entry.options)
	}
	switch {
	case entry != nil && !equalInt64Ptr(fstabPri, want):
		m.Priority = int64PtrValue(fstabPri)
	case live != nil && want != nil && live.priority != *want:
		m.Priority = types.Int64Value(live.priority)
	case imported && entry == nil && live != nil && live.priority >= 0:
		m.Priority = types.Int64Value(live.priority)
	}
	return diags
}

// reportedSizeMiB converts the size of a swap area in bytes to the value of
// size_mib. A size that is not a whole number of MiB is rounded away from
// want, so that it never reads as the configured size.
func reportedSizeMiB(bytes int64, want *int64) int64 {
	if bytes%mib == 0 || want == nil {
		return (bytes + mib - 1) / mib
	}
	if bytes > *want*mib {
		return bytes/mib + 1
	}
	return bytes / mib
}

func equalInt64Ptr(a, b *int64) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func int64PtrValue(v *int64) types.Int64 {
	if v == nil {
		return types.Int64Null()
	}
	return types.Int64Value(*v)
}

// formatBytes formats n bytes for messages.
func formatBytes(n int64) string {
	if n%mib == 0 {
		return fmt.Sprintf("%d MiB", n/mib)
	}
	return fmt.Sprintf("%d bytes", n)
}
