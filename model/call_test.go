package model

import (
	"strings"
	"testing"
)

func TestCallValidate(t *testing.T) {
	requireOK(t, Call{Correlation: "req-1"}.Validate())
	requireError(t, Call{}.Validate(), Validation, "correlation")
	requireError(t, Call{Correlation: "a\nb"}.Validate(), Validation, "correlation")
	requireError(t, Call{Correlation: CorrelationID(strings.Repeat("c", 256))}.Validate(), Validation, "correlation")
}

func TestCommandValidate(t *testing.T) {
	base := Command{Call: Call{Correlation: "req-1"}, Operation: "op-1"}
	withRev := base
	withRev.ExpectedRevision = "r1"
	badRev := base
	badRev.ExpectedRevision = " r1"

	cases := map[string]struct {
		cmd   Command
		rule  RevisionRule
		field string
	}{
		"optional without revision": {base, RevisionOptional, ""},
		"optional with revision":    {withRev, RevisionOptional, ""},
		"optional bad revision":     {badRev, RevisionOptional, "expected_revision"},
		"required with revision":    {withRev, RevisionRequired, ""},
		"required missing":          {base, RevisionRequired, "expected_revision"},
		"required bad revision":     {badRev, RevisionRequired, "expected_revision"},
		"absent without revision":   {base, RevisionAbsent, ""},
		"absent with revision":      {withRev, RevisionAbsent, "expected_revision"},
		"unknown rule":              {withRev, RevisionRule(99), "expected_revision"},
		"missing correlation":       {Command{Operation: "op-1"}, RevisionOptional, "correlation"},
		"missing operation":         {Command{Call: Call{Correlation: "c"}}, RevisionOptional, "operation"},
		"bad operation":             {Command{Call: Call{Correlation: "c"}, Operation: "op\x00"}, RevisionOptional, "operation"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			err := tc.cmd.Validate(tc.rule)
			if tc.field == "" {
				requireOK(t, err)
				return
			}
			requireError(t, err, Validation, tc.field)
		})
	}
}

func TestRevisionRuleZeroValueFailsClosed(t *testing.T) {
	var rule RevisionRule
	if rule != RevisionRequired {
		t.Fatal("zero RevisionRule must require a revision")
	}
}
