package examples

import (
	"testing"

	"hop.top/uma/model"
)

// The sample adapter follows the naming rules it illustrates: a third-party
// reverse-DNS type key and a configuration schema named "<type key>.config".
func TestSourceFactoryNames(t *testing.T) {
	d := SourceFactory{}.Describe()
	if err := model.ValidateTypeKey(d.Type); err != nil {
		t.Errorf("type %q: %v", d.Type, err)
	}
	if err := model.ValidateExtensionName(d.ConfigurationSchema.Name); err != nil {
		t.Errorf("configuration schema %q: %v", d.ConfigurationSchema.Name, err)
	}
	if want := string(d.Type) + ".config"; d.ConfigurationSchema.Name != want {
		t.Errorf("configuration schema = %q, want %q", d.ConfigurationSchema.Name, want)
	}
	if issues := (SourceFactory{}).Validate(d.ConfigurationSchema); len(issues) != 0 {
		t.Errorf("own configuration schema rejected: %v", issues)
	}
	for _, c := range d.Capabilities {
		if c != model.CapabilityReceive {
			t.Errorf("capability %q", c)
		}
	}
	for _, e := range d.Events {
		if e != model.EventMessageReceived {
			t.Errorf("event %q", e)
		}
	}
}
