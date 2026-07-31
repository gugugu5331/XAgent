package redact

// Redact removes static and runtime-registered secrets before constructing
// a SafeText value that may cross an output boundary.
func (r *RuntimeRedactor) Redact(raw string) SafeText {
	return SafeText{value: r.Text(raw)}
}
