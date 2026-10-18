package wire

import (
	"math"
	"reflect"
	"testing"

	"github.com/openfactory-org/openfactory/internal/core"
)

// The plugin protocol carries the factory of an integration record, so a
// plugin can prefer the record of the factory over the team record.
func TestIntegrationRoundTrip(t *testing.T) {
	in := core.Integration{ID: "int_1", TeamID: "team_1", Provider: "azure_devops", Name: "ado",
		Config: map[string]string{"org": "contoso"}, SecretRef: "ADO_TOKEN", FactoryID: "fac_1"}
	got := IntegrationFromPB(IntegrationToPB(in))
	if !reflect.DeepEqual(got, in) {
		t.Fatalf("round trip = %+v, want %+v", got, in)
	}
}

func TestClampInt32(t *testing.T) {
	for in, want := range map[int]int32{0: 0, 42: 42, -7: -7, math.MaxInt32 + 1: math.MaxInt32, math.MinInt32 - 1: math.MinInt32} {
		if got := ClampInt32(in); got != want {
			t.Errorf("ClampInt32(%d) = %d, want %d", in, got, want)
		}
	}
}
