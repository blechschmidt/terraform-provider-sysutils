package provider

import (
	"context"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

func TestValidateAbsolutePath(t *testing.T) {
	cases := []struct {
		in      string
		wantErr string // substring of the expected error; "" means valid
	}{
		{"/tmp", ""},
		{"/tmp/foo", ""},
		{"/etc/app.d/conf", ""},
		{"/a b/c", ""},
		{"", "must be absolute"},
		{"tmp", "must be absolute"},
		{"./tmp", "must be absolute"},
		{"../tmp", "must be absolute"},
		{"~/tmp", "must be absolute"},
		{"/tmp/", `use "/tmp"`},
		{"/tmp//foo", `use "/tmp/foo"`},
		{"/tmp/./foo", `use "/tmp/foo"`},
		{"/tmp/../etc", `use "/etc"`},
		{"//tmp", `use "/tmp"`},
		{"/", "filesystem root"},
		{"//", `use "/"`},
		{"/..", `use "/"`},
		{"/tmp/..", `use "/"`},
	}
	for _, c := range cases {
		err := validateAbsolutePath(c.in)
		switch {
		case c.wantErr == "" && err != nil:
			t.Errorf("validateAbsolutePath(%q) = %v, want nil", c.in, err)
		case c.wantErr != "" && err == nil:
			t.Errorf("validateAbsolutePath(%q) = nil, want error containing %q", c.in, c.wantErr)
		case c.wantErr != "" && !strings.Contains(err.Error(), c.wantErr):
			t.Errorf("validateAbsolutePath(%q) = %q, want error containing %q", c.in, err, c.wantErr)
		}
	}
}

func TestValidateOctalMode(t *testing.T) {
	valid := []string{"644", "755", "000", "777", "0644", "0755", "0000", "0777", "1777", "2755", "4755", "7777", "00644", "07777", "064"}
	invalid := []string{"", "0", "64", "0o644", "0x1ff", "888", "0648", "648", "77777", "17777", "000000", "+644", "-644", " 644", "644 ", "644\n", "rwxr-xr-x", "u+x"}

	for _, s := range valid {
		if err := validateOctalMode(s); err != nil {
			t.Errorf("validateOctalMode(%q) = %v, want nil", s, err)
		}
		// Everything the validator accepts must be usable by the resources.
		if _, err := parseMode(s); err != nil {
			t.Errorf("validateOctalMode accepts %q but parseMode rejects it: %v", s, err)
		}
	}
	for _, s := range invalid {
		if err := validateOctalMode(s); err == nil {
			t.Errorf("validateOctalMode(%q) = nil, want error", s)
		}
	}
}

func runStringValidator(v validator.String, value types.String) validator.StringResponse {
	req := validator.StringRequest{
		Path:        path.Root("attr"),
		ConfigValue: value,
	}
	var resp validator.StringResponse
	v.ValidateString(context.Background(), req, &resp)
	return resp
}

func TestStringValidators(t *testing.T) {
	cases := []struct {
		name    string
		v       validator.String
		value   types.String
		wantErr bool
	}{
		{"path null", absolutePath(), types.StringNull(), false},
		{"path unknown", absolutePath(), types.StringUnknown(), false},
		{"path valid", absolutePath(), types.StringValue("/srv/data"), false},
		{"path relative", absolutePath(), types.StringValue("srv/data"), true},
		{"path unclean", absolutePath(), types.StringValue("/srv/data/"), true},
		{"path root", absolutePath(), types.StringValue("/"), true},
		{"path or root valid", absolutePathOrRoot(), types.StringValue("/srv/data"), false},
		{"path or root root", absolutePathOrRoot(), types.StringValue("/"), false},
		{"path or root relative", absolutePathOrRoot(), types.StringValue("srv"), true},
		{"path or root unclean", absolutePathOrRoot(), types.StringValue("/srv/../etc"), true},
		{"mode null", octalMode(), types.StringNull(), false},
		{"mode unknown", octalMode(), types.StringUnknown(), false},
		{"mode valid", octalMode(), types.StringValue("0750"), false},
		{"mode invalid", octalMode(), types.StringValue("rwx"), true},
		{"mode too long", octalMode(), types.StringValue("77777"), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp := runStringValidator(c.v, c.value)
			if got := resp.Diagnostics.HasError(); got != c.wantErr {
				t.Fatalf("HasError() = %v, want %v; diagnostics: %v", got, c.wantErr, resp.Diagnostics)
			}
			if !c.wantErr {
				return
			}
			d := resp.Diagnostics.Errors()[0]
			withPath, ok := d.(interface{ Path() path.Path })
			if !ok || !withPath.Path().Equal(path.Root("attr")) {
				t.Errorf("diagnostic is not attached to the attribute path: %v", d)
			}
		})
	}
}

func TestValidatorDescriptions(t *testing.T) {
	ctx := context.Background()
	for _, v := range []validator.String{absolutePath(), absolutePathOrRoot(), octalMode()} {
		if v.Description(ctx) == "" || v.MarkdownDescription(ctx) == "" {
			t.Errorf("%T has an empty description", v)
		}
	}
}
