package prompts

import (
	"strings"
	"testing"
)

func TestSpliceSubstitutesEachToken(t *testing.T) {
	got := Splice("top\n\n"+TokenBuiltin+"\n\n"+TokenGraph+"\n\n"+TokenVerdict+"\n",
		Splices{Builtin: "BUILT", Graph: "GRAPHED", Verdict: "VERDICTED"})
	want := "top\n\nBUILT\n\nGRAPHED\n\nVERDICTED\n"
	if got != want {
		t.Fatalf("Splice:\n%q\nwant\n%q", got, want)
	}
}

// A splice with nothing to splice has to yield nothing: not the literal token,
// which would tell a model to read a section that is not there, and not an
// error, because a repo with no code index is the normal case rather than a
// misconfigured one.
func TestASpliceWithNothingToSpliceYieldsNothing(t *testing.T) {
	got := Splice("before\n\n"+TokenGraph+"\n\nafter\n", Splices{})
	if want := "before\n\nafter\n"; got != want {
		t.Fatalf("an empty splice left something behind:\n%q\nwant\n%q", got, want)
	}
	for _, tok := range []string{TokenBuiltin, TokenGraph, TokenVerdict} {
		if strings.Contains(got, tok) {
			t.Fatalf("%s survived into the prompt", tok)
		}
	}
}

// The shipped reviewer places the contract itself, so the text lives in exactly
// one file and a repo that materialises the reviewer gets a file that shows the
// splice in use.
func TestTheReviewerPromptPlacesTheVerdictContract(t *testing.T) {
	p, err := For("reviewer")
	if err != nil {
		t.Fatalf("For(reviewer): %v", err)
	}
	if !strings.Contains(p, TokenVerdict) {
		t.Fatal("the reviewer prompt no longer places {{VERDICT}}; the contract would have to be appended blind")
	}
	if !strings.Contains(Splice(p, Splices{Verdict: VerdictContract()}), "VERDICT: pass") {
		t.Fatal("the spliced reviewer prompt does not state the verdict line the parser reads")
	}
}

func TestVerdictContractStatesBothVerdicts(t *testing.T) {
	c := VerdictContract()
	for _, want := range []string{"VERDICT: pass", "VERDICT: fail"} {
		if !strings.Contains(c, want) {
			t.Fatalf("the verdict contract does not say %q", want)
		}
	}
}

// {{BUILTIN}} brings in text that has splices of its own, and a replacement is
// not rescanned, so it has to be expanded before the rest.
func TestBuiltinIsSplicedBeforeTheTokensItBringsIn(t *testing.T) {
	got := Splice(TokenBuiltin, Splices{
		Builtin: "shipped\n\n" + TokenVerdict,
		Verdict: "THE CONTRACT",
	})
	if want := "shipped\n\nTHE CONTRACT\n"; got != want {
		t.Fatalf("Splice:\n%q\nwant\n%q", got, want)
	}
}
