package store

import (
	"encoding/json"
	"regexp"
	"strings"
)

// AllowsAnonymous evaluates the bucket policy for an unauthenticated request: some statement must Allow
// the action on the resource for every principal, and no statement may Deny it. A statement this
// evaluator cannot fully judge (a Condition, NotAction, NotPrincipal, NotResource) never grants access,
// and a Deny of that kind is assumed to apply.
func (b BucketMeta) AllowsAnonymous(action, resource string) bool {
	if b.Policy == "" {
		return false
	}
	var doc struct {
		Statement json.RawMessage
	}
	if json.Unmarshal([]byte(b.Policy), &doc) != nil {
		return false
	}
	var stmts []policyStatement
	if json.Unmarshal(doc.Statement, &stmts) != nil {
		var one policyStatement
		if json.Unmarshal(doc.Statement, &one) != nil {
			return false
		}
		stmts = []policyStatement{one}
	}
	allowed := false
	for _, st := range stmts {
		partial := st.Condition != nil || st.NotAction != nil || st.NotPrincipal != nil || st.NotResource != nil
		switch {
		case st.Effect == "Deny" && (partial || st.matches(action, resource)):
			return false
		case st.Effect == "Allow" && !partial && st.matches(action, resource):
			allowed = true
		}
	}
	return allowed
}

type policyStatement struct {
	Effect                                          string
	Principal                                       json.RawMessage
	Action, Resource                                stringList
	Condition, NotAction, NotPrincipal, NotResource json.RawMessage
}

// matches reports whether the statement names every principal ("*", {"AWS":"*"} or {"AWS":["*"]}), the
// action and the resource, with IAM's * and ? wildcards.
func (st policyStatement) matches(action, resource string) bool {
	everyone := string(st.Principal) == `"*"`
	var p struct{ AWS stringList }
	if !everyone && json.Unmarshal(st.Principal, &p) == nil {
		for _, v := range p.AWS {
			everyone = everyone || v == "*"
		}
	}
	return everyone && st.Action.match(action, true) && st.Resource.match(resource, false)
}

// stringList is a policy field that may be a single string or an array of them.
type stringList []string

func (l *stringList) UnmarshalJSON(b []byte) error {
	var one string
	if json.Unmarshal(b, &one) == nil {
		*l = stringList{one}
		return nil
	}
	return json.Unmarshal(b, (*[]string)(l))
}

func (l stringList) match(s string, fold bool) bool {
	for _, pat := range l {
		expr := "^" + strings.NewReplacer(`\*`, ".*", `\?`, ".").Replace(regexp.QuoteMeta(pat)) + "$"
		if fold {
			expr = "(?i)" + expr // action names are case-insensitive; resources are not
		}
		if ok, _ := regexp.MatchString(expr, s); ok {
			return true
		}
	}
	return false
}
