package agentrole

import "embed"

//go:embed builtins/*.md
var builtinRoles embed.FS

func BuiltinSource() FileSource {
	return FileSource{Source: SourceBuiltin, ID: "builtin", FS: builtinRoles, Root: "builtins"}
}

func BuiltinFileSource() FileSource {
	return BuiltinSource()
}
