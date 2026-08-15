package tui

import (
	"testing"

	"xagent/internal/redact"
)

func TestConfirmationViewStructuredScopeIsSafeAndImmutable(t *testing.T) {
	redactor := redact.NewRuntimeRedactor()
	target := redactor.Redact("/workspace/output.txt")
	ruleLocation := redactor.Redact(".xagent/permissions.local.yaml")
	description := redactor.Redact("仅允许本会话写入目标文件")
	scopes := []ConfirmationScopeViewSpec{{
		Scope: "session", Available: true, Description: description,
	}}

	view := NewStateViewModel(ViewModelSpec{
		Request: RequestViewSpec{Confirmation: ConfirmationViewSpec{
			Present: true, CallID: "call-structured", Name: "Write",
			Target: target, RuleLocation: ruleLocation, Scopes: scopes,
		}},
	})

	// Construction must detach the view from the App-owned input slice.
	scopes[0] = ConfirmationScopeViewSpec{
		Scope: "mutated", Description: redactor.Redact("mutated description"),
	}

	request := view.Request()
	confirmation, present := request.Confirmation()
	gotScopes := confirmation.Scopes()
	if !present || confirmation.Target().Text() != target.Text() ||
		confirmation.RuleLocation().Text() != ruleLocation.Text() || len(gotScopes) != 1 ||
		gotScopes[0].Scope() != "session" || !gotScopes[0].Available() ||
		gotScopes[0].Description().Text() != description.Text() {
		t.Fatalf("structured confirmation projection changed: present=%t confirmation=%#v scopes=%#v", present, confirmation, gotScopes)
	}

	// Both nested getters and ViewModel.Request must return detached slices.
	gotScopes[0] = ConfirmationScopeView{}
	detachedRequest := view.Request()
	detachedRequest.confirmation.scopes[0] = ConfirmationScopeView{}

	again, present := view.Request().Confirmation()
	againScopes := again.Scopes()
	if !present || len(againScopes) != 1 || againScopes[0].Scope() != "session" ||
		!againScopes[0].Available() || againScopes[0].Description().Text() != description.Text() {
		t.Fatalf("confirmation exposed renderer-mutable scope storage: %#v", againScopes)
	}
}

func TestConfirmationViewScopesPreservePresence(t *testing.T) {
	nilConfirmation, present := NewStateViewModel(ViewModelSpec{
		Request: RequestViewSpec{Confirmation: ConfirmationViewSpec{Present: true}},
	}).Request().Confirmation()
	if !present || nilConfirmation.Scopes() != nil {
		t.Fatalf("nil scopes changed presence: present=%t scopes=%#v", present, nilConfirmation.Scopes())
	}

	emptyConfirmation, present := NewStateViewModel(ViewModelSpec{
		Request: RequestViewSpec{Confirmation: ConfirmationViewSpec{
			Present: true, Scopes: []ConfirmationScopeViewSpec{},
		}},
	}).Request().Confirmation()
	if !present || emptyConfirmation.Scopes() == nil || len(emptyConfirmation.Scopes()) != 0 {
		t.Fatalf("present empty scopes changed presence: present=%t scopes=%#v", present, emptyConfirmation.Scopes())
	}
}
