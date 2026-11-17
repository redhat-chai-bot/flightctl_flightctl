package flterrors

import "errors"

var (
	ErrImmutableCertificateBinding  = errors.New("certificateName is immutable")
	ErrEnrollmentCredentialRetained = errors.New("enrollment credential is retained by policy bindings or audit history")
)
