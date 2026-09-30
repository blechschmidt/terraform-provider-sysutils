package provider

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework-validators/listvalidator"
	"github.com/hashicorp/terraform-plugin-framework/ephemeral"
	"github.com/hashicorp/terraform-plugin-framework/ephemeral/schema"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

const (
	// defaultEphemeralExecTimeout is the default timeout of the sysutils_exec
	// ephemeral resource. Unlike the resource, it runs during every plan, so
	// a hanging command must not hang planning forever.
	defaultEphemeralExecTimeout = 5 * time.Minute
	// maxEphemeralOutputBytes is the largest max_output_bytes allowed for the
	// sysutils_exec ephemeral resource.
	maxEphemeralOutputBytes = 16 << 20
	// maxEphemeralStderrInDiag bounds how much standard error a failure
	// diagnostic quotes.
	maxEphemeralStderrInDiag = 4096
)

var _ ephemeral.EphemeralResource = (*execEphemeralResource)(nil)

func NewExecEphemeralResource() ephemeral.EphemeralResource { return &execEphemeralResource{} }

type execEphemeralResource struct{}

type execEphemeralModel struct {
	Command                  types.List   `tfsdk:"command"`
	Environment              types.Map    `tfsdk:"environment"`
	InheritParentEnvironment types.Bool   `tfsdk:"inherit_parent_environment"`
	WorkingDirectory         types.String `tfsdk:"working_directory"`
	Stdin                    types.String `tfsdk:"stdin"`
	Timeout                  types.String `tfsdk:"timeout"`
	FailOnNonzero            types.Bool   `tfsdk:"fail_on_nonzero"`
	MaxOutputBytes           types.Int64  `tfsdk:"max_output_bytes"`
	ExitCode                 types.Int64  `tfsdk:"exit_code"`
	Stdout                   types.String `tfsdk:"stdout"`
	Stderr                   types.String `tfsdk:"stderr"`
}

func (e *execEphemeralResource) Metadata(_ context.Context, req ephemeral.MetadataRequest, resp *ephemeral.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_exec"
}

func (e *execEphemeralResource) Schema(_ context.Context, _ ephemeral.SchemaRequest, resp *ephemeral.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Runs a command and returns its exit code, standard output and standard error without storing them in the plan or the state. " +
			"Use it to fetch a secret from a password manager, vault CLI or key file and pass it to write-only arguments such as `content_wo` of the `sysutils_file` resource. " +
			"The command runs whenever Terraform opens the ephemeral resource, which is during every plan and apply that needs it, so it must be safe to run repeatedly and should not change anything. " +
			"It runs on the host, as the provider's user, also when the provider's `root_dir` is set.",
		Attributes: map[string]schema.Attribute{
			"command": schema.ListAttribute{
				Required:            true,
				ElementType:         types.StringType,
				MarkdownDescription: "Command and arguments as a list. Element `0` is the executable, looked up in `PATH` if it contains no slash. Example: `[\"/bin/sh\", \"-c\", \"pass show app/token\"]`.",
				Validators:          []validator.List{listvalidator.SizeAtLeast(1)},
			},
			"environment": schema.MapAttribute{
				Optional:    true,
				ElementType: types.StringType,
				MarkdownDescription: "Environment variables to pass to the command. " +
					"If `inherit_parent_environment` is `false`, these are the only variables in its environment.",
			},
			"inherit_parent_environment": schema.BoolAttribute{
				Optional: true,
				MarkdownDescription: "If `true` (default), the command starts from the Terraform provider's environment and the keys in `environment` override specific values. " +
					"If `false`, only the `environment` map is used.",
			},
			"working_directory": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Working directory of the command. Defaults to the provider's working directory.",
			},
			"stdin": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Data to pipe into the command's standard input.",
			},
			"timeout": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "Maximum run time, as a Go duration string such as `\"30s\"` or `\"2m\"`. Defaults to `\"5m\"`. " +
					"When it expires, the command's whole process group is killed with `SIGKILL` and the operation fails, regardless of `fail_on_nonzero`.",
				Validators: []validator.String{positiveDuration()},
			},
			"fail_on_nonzero": schema.BoolAttribute{
				Optional: true,
				MarkdownDescription: "If `true` (default), a non-zero exit code fails the operation. The diagnostic quotes the beginning of standard error, never standard output. " +
					"If `false`, the exit code is returned in `exit_code`.",
			},
			"max_output_bytes": schema.Int64Attribute{
				Optional: true,
				MarkdownDescription: fmt.Sprintf("Maximum number of bytes of standard output, and separately of standard error, to accept. Defaults to `%d` (1 MiB); at most `%d` (16 MiB). ",
					defaultMaxOutputBytes, maxEphemeralOutputBytes) +
					"More output is an error rather than being truncated, so that a secret is never silently cut short.",
				Validators: []validator.Int64{int64validator.Between(0, maxEphemeralOutputBytes)},
			},
			"exit_code": schema.Int64Attribute{
				Computed:            true,
				MarkdownDescription: "Exit code of the command.",
			},
			"stdout": schema.StringAttribute{
				Computed:  true,
				Sensitive: true,
				MarkdownDescription: "Standard output, verbatim (use `trimspace()` to drop a trailing newline). " +
					"Bytes that are not valid UTF-8 are replaced by U+FFFD.",
			},
			"stderr": schema.StringAttribute{
				Computed:            true,
				Sensitive:           true,
				MarkdownDescription: "Standard error, verbatim. Bytes that are not valid UTF-8 are replaced by U+FFFD.",
			},
		},
	}
}

func (e *execEphemeralResource) Open(ctx context.Context, req ephemeral.OpenRequest, resp *ephemeral.OpenResponse) {
	var m execEphemeralModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}

	var spec execSpec
	resp.Diagnostics.Append(m.Command.ElementsAs(ctx, &spec.Argv, false)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if len(spec.Argv) == 0 {
		resp.Diagnostics.AddAttributeError(path.Root("command"), "Empty command", "The command list must contain at least one element.")
		return
	}
	extraEnv := map[string]string{}
	if !m.Environment.IsNull() {
		resp.Diagnostics.Append(m.Environment.ElementsAs(ctx, &extraEnv, false)...)
		if resp.Diagnostics.HasError() {
			return
		}
	}
	spec.Env = buildEnv(m.InheritParentEnvironment.IsNull() || m.InheritParentEnvironment.ValueBool(), extraEnv)
	spec.Dir = m.WorkingDirectory.ValueString()
	spec.Stdin = m.Stdin.ValueString()
	spec.Timeout = defaultEphemeralExecTimeout
	if t := m.Timeout.ValueString(); t != "" {
		d, err := time.ParseDuration(t)
		if err != nil || d <= 0 {
			resp.Diagnostics.AddAttributeError(path.Root("timeout"), "Invalid timeout", fmt.Sprintf("Timeout %q is not a positive duration.", t))
			return
		}
		spec.Timeout = d
	}
	spec.MaxOutputBytes = defaultMaxOutputBytes
	if !m.MaxOutputBytes.IsNull() {
		spec.MaxOutputBytes = m.MaxOutputBytes.ValueInt64()
	}
	if spec.MaxOutputBytes < 0 || spec.MaxOutputBytes > maxEphemeralOutputBytes {
		resp.Diagnostics.AddAttributeError(path.Root("max_output_bytes"), "Invalid max_output_bytes",
			fmt.Sprintf("max_output_bytes must be between 0 and %d.", maxEphemeralOutputBytes))
		return
	}

	res, err := runCommand(ctx, spec)
	if err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("command"), "Command failed to run", capitalize(err.Error())+".")
		return
	}
	if res.TimedOut {
		resp.Diagnostics.AddAttributeError(path.Root("command"), fmt.Sprintf("Command timed out after %s", spec.Timeout),
			fmt.Sprintf("%s did not finish within the timeout; its process group was killed.%s", spec.Argv[0], quoteStderr(res)))
		return
	}
	if res.Truncated() {
		resp.Diagnostics.AddAttributeError(path.Root("max_output_bytes"), "Command output too large",
			fmt.Sprintf("%s wrote %d bytes to standard output and %d bytes to standard error, more than max_output_bytes (%d) allows. "+
				"Raise max_output_bytes or reduce the output.", spec.Argv[0], res.Stdout.total, res.Stderr.total, spec.MaxOutputBytes))
		return
	}
	if (m.FailOnNonzero.IsNull() || m.FailOnNonzero.ValueBool()) && res.ExitCode != 0 {
		resp.Diagnostics.AddAttributeError(path.Root("command"), fmt.Sprintf("Command exited with non-zero status %d", res.ExitCode),
			fmt.Sprintf("Standard output is withheld because it may hold a secret. Set fail_on_nonzero = false to handle the exit code in the configuration.%s", quoteStderr(res)))
		return
	}

	m.ExitCode = types.Int64Value(int64(res.ExitCode))
	// Terraform strings must be valid UTF-8.
	m.Stdout = types.StringValue(strings.ToValidUTF8(res.Stdout.String(), "�"))
	m.Stderr = types.StringValue(strings.ToValidUTF8(res.Stderr.String(), "�"))
	resp.Diagnostics.Append(resp.Result.Set(ctx, &m)...)
}

// quoteStderr renders the beginning of the command's standard error for a
// diagnostic, or nothing if it is empty.
func quoteStderr(res *execResult) string {
	s := res.Stderr.buf.Bytes()
	if len(s) == 0 {
		return ""
	}
	cut := ""
	if len(s) > maxEphemeralStderrInDiag {
		s, cut = trimPartialRune(s[:maxEphemeralStderrInDiag]), "\n[...]"
	}
	return "\n\nstderr:\n" + strings.ToValidUTF8(string(s), "�") + cut
}
