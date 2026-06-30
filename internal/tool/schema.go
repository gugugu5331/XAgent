package tool

func ObjectSchema(required []string, properties map[string]SchemaProperty) Schema {
	return Schema{Type: "object", Required: required, Properties: properties}
}

func StringProperty(description string) SchemaProperty {
	return SchemaProperty{Type: "string", Description: description}
}

func BoolProperty(description string) SchemaProperty {
	return SchemaProperty{Type: "boolean", Description: description}
}

func EnumProperty(description string, values ...string) SchemaProperty {
	return SchemaProperty{Type: "string", Description: description, Enum: values}
}
