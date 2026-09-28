package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestDotEnvLiteralValues(t *testing.T) {
	t.Parallel()
	body := "\ufeff# comment\r\n export USER = user # trailing\r\n" +
		"SINGLE='SECRET $NAME ${MISSING} $$ # literal'\n" +
		"DOUBLE=\"SECRET $NAME ${MISSING} $$ # literal\" # trailing\n" +
		"ESCAPED=\"a\\n\\r\\t\\\\\\\"\\$b\"\n" +
		"RAW=SECRET$NAME#hash # comment\nEMPTY=\nEMPTY_COMMENT= # comment\n" +
		"SINGLE_EMPTY=''\nDOUBLE_EMPTY=\"\"\nEQUALS=a=b=c\nSPACES='  value  '\n" +
		"LITERAL_ESCAPES='a\\nb'\nDUPLICATE=old\nDUPLICATE=new\n" +
		"COMMAND=$(exit 1)\nBACKTICKS=`exit 1`\n"
	values, err := parseDotEnv([]byte(body))
	want := map[string]string{
		"USER": "user", "SINGLE": "SECRET $NAME ${MISSING} $$ # literal", "DOUBLE": "SECRET $NAME ${MISSING} $$ # literal",
		"ESCAPED": "a\n\r\t\\\"$b", "RAW": "SECRET$NAME#hash", "EMPTY": "", "EMPTY_COMMENT": "", "SINGLE_EMPTY": "", "DOUBLE_EMPTY": "",
		"EQUALS": "a=b=c", "SPACES": "  value  ", "LITERAL_ESCAPES": `a\nb`, "DUPLICATE": "new", "COMMAND": "$(exit 1)", "BACKTICKS": "`exit 1`",
	}
	if err != nil || !reflect.DeepEqual(values, want) {
		t.Fatal("dotenv literal parsing failed", err)
	}
	for _, name := range []string{"SINGLE", "DOUBLE", "RAW", "ESCAPED"} {
		c, err := DecodeWithEnvironment(YAML, []byte(minimal+"qbt_username: user\nqbt_password: '${"+name+"}'\n"), values)
		if err != nil || c.Password != want[name] {
			t.Fatal("dotenv secret content was expanded recursively", err)
		}
	}
}

func TestInvalidDotEnvIsSanitizedAndNeverIgnored(t *testing.T) {
	t.Parallel()
	for _, body := range []string{
		"SECRET", "export SECRET", "=SECRET", "1KEY=SECRET", "BAD-KEY=SECRET", "BAD.KEY=SECRET", "BAD KEY=SECRET",
		"KEY='SECRET", "KEY=\"SECRET", "KEY='SECRET' trailing", "KEY=\"SECRET\" other=value", "KEY=\"SECRET\\q\"",
		"KEY=\"SECRET\\", "KEY=unquoted'SECRET'", "KEY='SECRET\ncontinued'", "KEY=SECRET\x00", "KEY=SECRET\xff", "KEY=SECRET\rVALUE",
	} {
		if _, err := parseDotEnv([]byte(body)); err == nil || err.Error() != "invalid dotenv syntax" {
			t.Fatal("invalid dotenv accepted or leaked", err)
		}
	}
	path := writeFile(t, "config.yaml", minimal)
	dotenv := filepath.Join(filepath.Dir(path), ".env")
	for _, body := range []string{"SECRET_INVALID_SYNTAX", strings.Repeat("#", maxDotEnvBytes+1)} {
		putFixture(t, dotenv, body)
		_, err := LoadWithEnvironment(path, map[string]string{"KEY": "process"})
		if err == nil || strings.Contains(err.Error(), "SECRET") || strings.Contains(err.Error(), dotenv) {
			t.Fatal("unused invalid/oversized dotenv was ignored or leaked", err)
		}
	}
	putFixture(t, dotenv, "#"+strings.Repeat("x", maxDotEnvBytes-1))
	if _, err := LoadWithEnvironment(path, nil); err != nil {
		t.Fatal("exact size limit rejected", err)
	}
	if err := os.Remove(dotenv); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(dotenv, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadWithEnvironment(path, nil); err == nil || strings.Contains(err.Error(), dotenv) {
		t.Fatal("unreadable dotenv accepted or leaked", err)
	}
	if err := os.Remove(dotenv); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(t.TempDir(), "MISSING_SECRET"), dotenv); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadWithEnvironment(path, nil); err == nil || strings.Contains(err.Error(), "SECRET") {
		t.Fatal("broken dotenv symlink accepted or leaked", err)
	}
}
