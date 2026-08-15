package command

type Controller interface {
	DisplayNotice(text string)
	DisplayError(err error)
	SendUserMessage(text string)
	ExecuteSkill(name string, args string, raw string) error
	OpenArtifact(id string) error

	ClearMessages()
	SwitchMode(mode Mode)
	CurrentMode() Mode
	RefreshStatus()

	CompactContext() (string, error)
	SessionStatus() SessionStatus
	MemoryStatus() MemoryStatus
	PermissionStatus() PermissionStatus
	RuntimeStatus() RuntimeStatus
	TokenUsage() TokenUsage

	LegacyMemory(args string) (string, error)
	LegacyMCPStatus() (string, error)
	LegacyDiagnostics() (string, error)
}
