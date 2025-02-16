package converter

import (
	"log/slog"
	"sort"

	"github.com/sudorandom/protoc-gen-connect-openapi/internal/converter/gnostic"
	"github.com/sudorandom/protoc-gen-connect-openapi/internal/converter/protovalidate"
	"github.com/swaggest/jsonschema-go"
	"google.golang.org/protobuf/reflect/protoreflect"
)

type State struct {
	Opts        Options
	CurrentFile protoreflect.FileDescriptor
	Messages    map[protoreflect.MessageDescriptor]struct{}
	Enums       map[protoreflect.EnumDescriptor]struct{}
}

func NewState(opts Options) *State {
	return &State{
		Opts:     opts,
		Messages: map[protoreflect.MessageDescriptor]struct{}{},
		Enums:    map[protoreflect.EnumDescriptor]struct{}{},
	}
}

func (st *State) CollectFile(tt protoreflect.FileDescriptor) {
	st.CurrentFile = tt

	// Files can have enums
	enums := tt.Enums()
	for i := 0; i < enums.Len(); i++ {
		st.CollectEnum(enums.Get(i))
	}

	// Files can have messages
	messages := tt.Messages()
	for i := 0; i < messages.Len(); i++ {
		st.CollectMessage(messages.Get(i))
	}

	// Also make sure to pick up messages referenced in service methods
	services := tt.Services()
	for i := 0; i < services.Len(); i++ {
		service := services.Get(i)
		methods := service.Methods()
		for j := 0; j < methods.Len(); j++ {
			method := methods.Get(j)
			st.CollectMessage(method.Input())
			st.CollectMessage(method.Output())
		}
	}
}

func (st *State) CollectEnum(tt protoreflect.EnumDescriptor) {
	if tt == nil {
		return
	}
	// Make sure we're not recursing through the same enum a second time
	if _, ok := st.Enums[tt]; ok {
		return
	}
	st.Enums[tt] = struct{}{}
}

func (st *State) CollectMessage(tt protoreflect.MessageDescriptor) {
	if tt == nil {
		return
	}
	// Make sure we're not recursing through the same message a second time
	if _, ok := st.Messages[tt]; ok {
		return
	}
	st.Messages[tt] = struct{}{}

	// Messages can have fields
	fields := tt.Fields()
	for i := 0; i < fields.Len(); i++ {
		st.CollectField(fields.Get(i))
	}

	// Messages can have enums
	enums := tt.Enums()
	for i := 0; i < enums.Len(); i++ {
		st.CollectEnum(enums.Get(i))
	}

	// Messages can have messages
	messages := tt.Messages()
	for i := 0; i < messages.Len(); i++ {
		message := messages.Get(i)
		st.CollectMessage(message)
	}
}

func (st *State) CollectField(tt protoreflect.FieldDescriptor) {
	if tt == nil {
		return
	}
	st.CollectEnum(tt.Enum())
	st.CollectMessage(tt.Message())
	st.CollectField(tt.MapKey())
	st.CollectField(tt.MapValue())
}

func (st *State) SortedEnums() []protoreflect.EnumDescriptor {
	enums := make([]protoreflect.EnumDescriptor, 0, len(st.Enums))
	for enum := range st.Enums {
		enums = append(enums, enum)
	}
	sort.Slice(enums, func(i, j int) bool {
		return enums[i].FullName() < enums[j].FullName()
	})
	return enums
}

func (st *State) SortedMessages() []protoreflect.MessageDescriptor {
	messages := make([]protoreflect.MessageDescriptor, 0, len(st.Messages))
	for message := range st.Messages {
		messages = append(messages, message)
	}
	sort.Slice(messages, func(i, j int) bool {
		return messages[i].FullName() < messages[j].FullName()
	})
	return messages
}

func enumToSchema(state *State, tt protoreflect.EnumDescriptor) *jsonschema.Schema {
	slog.Debug("enumToSchema", slog.Any("descriptor", tt.FullName()))
	s := &jsonschema.Schema{}
	s.WithID(string(tt.FullName()))
	s.WithTitle(string(tt.Name()))
	s.WithDescription(formatComments(tt.ParentFile().SourceLocations().ByDescriptor(tt)))
	s.WithType(jsonschema.String.Type())
	children := []interface{}{}
	values := tt.Values()
	for i := 0; i < values.Len(); i++ {
		value := values.Get(i)
		children = append(children, string(value.Name()))
		if !state.Opts.OnlyStringEnumValues {
			children = append(children, value.Number())
		}
	}
	s.WithEnum(children)
	return s
}

func messageToSchema(state *State, tt protoreflect.MessageDescriptor) *jsonschema.Schema {
	slog.Debug("messageToSchema", slog.Any("descriptor", tt.FullName()))
	if isWellKnown(tt) {
		return wellKnownToSchema(tt)
	}
	s := &jsonschema.Schema{}
	s.WithID(string(tt.FullName()))
	s.WithTitle(string(tt.Name()))
	s.WithDescription(formatComments(tt.ParentFile().SourceLocations().ByDescriptor(tt)))
	s.WithType(jsonschema.Object.Type())
	s.WithAdditionalProperties(jsonschema.SchemaOrBool{TypeBoolean: BoolPtr(false)})

	fields := tt.Fields()
	children := make(map[string]jsonschema.SchemaOrBool, fields.Len())
	for i := 0; i < fields.Len(); i++ {
		field := fields.Get(i)
		child := fieldToSchema(state, s, field)
		children[field.JSONName()] = jsonschema.SchemaOrBool{TypeObject: child}
	}

	s.WithProperties(children)

	// Apply Updates from Options
	s = protovalidate.SchemaWithMessageAnnotations(s, tt)
	s = gnostic.SchemaWithSchemaAnnotations(s, tt)
	return s
}

func fieldToSchema(state *State, parent *jsonschema.Schema, tt protoreflect.FieldDescriptor) *jsonschema.Schema {
	slog.Debug("fieldToSchema", slog.Any("descriptor", tt.FullName()))
	s := &jsonschema.Schema{Parent: parent}

	// TODO: 64-bit types can be strings or numbers because they sometimes
	//       cannot fit into a JSON number type
	switch tt.Kind() {
	case protoreflect.BoolKind:
		s.WithType(jsonschema.Boolean.Type())
	case protoreflect.EnumKind:
		s.WithRef("#/components/schemas/" + string(tt.Enum().FullName()))
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Uint32Kind,
		protoreflect.Sfixed32Kind, protoreflect.Fixed32Kind:
		s.WithType(jsonschema.Integer.Type())
	case protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Uint64Kind,
		protoreflect.Sfixed64Kind, protoreflect.Fixed64Kind, protoreflect.DoubleKind:
		s.WithOneOf(
			jsonschema.SchemaOrBool{TypeObject: (&jsonschema.Schema{}).WithType(jsonschema.String.Type())},
			jsonschema.SchemaOrBool{TypeObject: (&jsonschema.Schema{}).WithType(jsonschema.Number.Type())},
		)
	case protoreflect.FloatKind:
		s.WithType(jsonschema.Number.Type())
	case protoreflect.StringKind:
		s.WithType(jsonschema.String.Type())
	case protoreflect.BytesKind:
		s.WithType(jsonschema.String.Type())
		s.WithFormat("byte")
	case protoreflect.MessageKind:
		s.WithRef("#/components/schemas/" + string(tt.Message().FullName()))
		s.WithType(jsonschema.Object.Type())
	}

	// Handle maps
	if tt.IsMap() {
		s.AdditionalProperties = &jsonschema.SchemaOrBool{TypeObject: fieldToSchema(state, s, tt.MapValue())}
		s.WithType(jsonschema.Object.Type())
		s.Ref = nil
	}

	// Handle Lists
	if tt.IsList() {
		wrapped := s
		s = &jsonschema.Schema{}
		wrapped.Parent = s
		s.WithType(jsonschema.Array.Type())
		s.WithItems(jsonschema.Items{SchemaOrBool: &jsonschema.SchemaOrBool{TypeObject: wrapped}})
	}

	s.WithTitle(string(tt.Name()))
	s.WithDescription(formatComments(tt.ParentFile().SourceLocations().ByDescriptor(tt)))
	s.WithAdditionalProperties(jsonschema.SchemaOrBool{TypeBoolean: BoolPtr(false)})

	// Apply Updates from Options
	s = protovalidate.SchemaWithFieldAnnotations(s, tt)
	s = gnostic.SchemaWithPropertyAnnotations(s, tt)
	return s
}

func stateToSchema(st *State) *jsonschema.Schema {
	fd := st.CurrentFile
	s := &jsonschema.Schema{}
	s.WithID(string(fd.FullName()))
	s.WithTitle(string(fd.Name()))
	s.WithDescription(formatComments(fd.ParentFile().SourceLocations().ByDescriptor(fd)))

	children := []jsonschema.SchemaOrBool{}
	for _, enum := range st.SortedEnums() {
		children = append(children, jsonschema.SchemaOrBool{TypeObject: enumToSchema(st, enum)})
	}

	for _, message := range st.SortedMessages() {
		children = append(children, jsonschema.SchemaOrBool{TypeObject: messageToSchema(st, message)})
	}

	s.WithItems(jsonschema.Items{SchemaArray: children})
	return s
}
