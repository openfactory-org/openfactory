package definition

import "testing"

func TestRequireLinkedActor(t *testing.T) {
	def := mustCompile(t, defTree("factory.yaml", factoryWith("requireLinkedActor: true\n")))
	if !def.RequireLinkedActor {
		t.Fatal("requireLinkedActor not compiled")
	}
	if plain := mustCompile(t, defTree("factory.yaml", factoryWith(""))); plain.RequireLinkedActor {
		t.Fatal("requireLinkedActor must default to false")
	}
	_, errs := Compile(defTree("factory.yaml", factoryWith("requireLinkedActor: maybe\n")))
	if len(errs) == 0 {
		t.Fatal("a value that is not a boolean must fail")
	}
}
