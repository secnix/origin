package scopeauthorizer

import (
	"fmt"

	"k8s.io/apiserver/pkg/authorization/authorizer"
	rbaclisters "k8s.io/client-go/listers/rbac/v1"
	authorizerrbac "k8s.io/kubernetes/plugin/pkg/auth/authorizer/rbac"

	authorizationapi "github.com/openshift/origin/pkg/authorization/apis/authorization"
	"github.com/openshift/origin/pkg/authorization/authorizer/scope"
)

type scopeAuthorizer struct {
	clusterRoleGetter rbaclisters.ClusterRoleLister
}

func NewAuthorizer(clusterRoleGetter rbaclisters.ClusterRoleLister) authorizer.Authorizer {
	return &scopeAuthorizer{clusterRoleGetter: clusterRoleGetter}
}

func (a *scopeAuthorizer) Authorize(attributes authorizer.Attributes) (authorizer.Decision, string, error) {
	user := attributes.GetUser()
	if user == nil {
		return authorizer.DecisionNoOpinion, "", fmt.Errorf("user missing from context")
	}

	scopes := user.GetExtra()[authorizationapi.ScopesKey]
	if len(scopes) == 0 {
		return authorizer.DecisionNoOpinion, "", nil
	}

	nonFatalErrors := ""

	// scopeResolutionErrors aren't fatal.  If any of the scopes we find allow this, then the overall scope limits allow it
	rules, err := scope.ScopesToRules(scopes, attributes.GetNamespace(), a.clusterRoleGetter)
	if err != nil {
		nonFatalErrors = fmt.Sprintf(", additionally the following non-fatal errors were reported: %v", err)
	}

	// check rules against attributes
	if authorizerrbac.RulesAllow(attributes, rules...) {
		return authorizer.DecisionNoOpinion, "", nil
	}

	// the scope prevent this.  We need to authoritatively deny
	return authorizer.DecisionDeny, fmt.Sprintf("scopes %v prevent this action%s", scopes, nonFatalErrors), nil
}
