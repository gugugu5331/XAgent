package sessionctx

import (
	"context"
	"testing"
)

type stablePreparerFixture struct{}

func (stablePreparerFixture) PrepareStable(context.Context) PreparedContext {
	return PreparedContext{}
}

func TestStablePreparerContract(t *testing.T) {
	var _ StablePreparer = (*Manager)(nil)
	var preparer StablePreparer = stablePreparerFixture{}
	if got := preparer.PrepareStable(context.Background()); len(got.StableSections) != 0 {
		t.Fatalf("empty stable context = %#v", got)
	}
}
