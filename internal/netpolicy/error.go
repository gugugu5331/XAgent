package netpolicy

type ErrorCode string

const (
	CodeInvalidRequest       ErrorCode = "invalid_request"
	CodeInvalidPurpose       ErrorCode = "invalid_purpose"
	CodeInvalidEndpoint      ErrorCode = "invalid_endpoint"
	CodeURLUserinfoForbidden ErrorCode = "url_userinfo_forbidden"
	CodeSchemeForbidden      ErrorCode = "scheme_forbidden"
	CodePlainHTTPForbidden   ErrorCode = "plain_http_forbidden"
	CodePolicyUnavailable    ErrorCode = "policy_unavailable"
)

type Error struct {
	Code ErrorCode
}

func (e *Error) Error() string {
	if e == nil || e.Code == "" {
		return "netpolicy_error"
	}
	return "netpolicy_" + string(e.Code)
}

func policyError(code ErrorCode) error {
	return &Error{Code: code}
}
