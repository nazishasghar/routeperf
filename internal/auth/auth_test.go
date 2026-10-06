package auth

import (
	"testing"

	"gopkg.in/yaml.v3"
)

func TestConfigRoles(t *testing.T) {
	var c Config
	err := yaml.Unmarshal([]byte(`
bearer: cust
headers: { X-Tenant-Id: acme }
roles:
  admin:
    login: { path: /auth/login, body: { email: a@x.dev }, extract: { bearer_from: $.token } }
    ops: [tag:admin, "/admin/**"]
  driver:
    bearer: drv
`), &c)
	if err != nil {
		t.Fatal(err)
	}
	if c.Bearer != "cust" || c.Headers["X-Tenant-Id"] != "acme" {
		t.Errorf("default identity = %+v", c.Creds)
	}
	adm, drv := c.Roles["admin"], c.Roles["driver"]
	if adm.Login == nil || adm.Login.Path != "/auth/login" || len(adm.Ops) != 2 || !adm.Dynamic() {
		t.Errorf("admin = %+v", adm)
	}
	if drv.Bearer != "drv" || drv.Dynamic() || drv.Empty() {
		t.Errorf("driver = %+v", drv)
	}
	if !(Creds{}).Empty() || (Creds{Query: map[string]string{"k": "v"}}).Empty() {
		t.Error("Empty")
	}
	out, err := yaml.Marshal(&c)
	if err != nil {
		t.Fatal(err)
	}
	var back Config
	if err := yaml.Unmarshal(out, &back); err != nil || back.Roles["admin"].Login.Path != "/auth/login" || back.Bearer != "cust" {
		t.Errorf("round trip:\n%s", out)
	}
}
