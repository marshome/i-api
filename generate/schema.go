package generate

import (
	"fmt"
	"log"
	"os"

	"github.com/marshome/apis/spec"
)

type Schema struct {
	c *Context

	m *spec.APIObject

	typ *Type // lazily populated by Type

	apiName      string // the native API-defined name of this type
	goName       string // lazily populated by GoName
	goReturnType string // lazily populated by GoReturnType

	properties []*Property
}

func NewSchema(ctx *Context,spec *spec.APIObject,apiName string, typ  *Type) (s*Schema) {
	s = &Schema{
		c:ctx,
		m:spec,
		apiName:apiName,
		typ:typ,
	}

	if s.typ == nil {
		s.typ = &Type{c: s.c, m: s.m, _apiName: s.apiName}
	}

	if name, ok := s.Type().MapType(); ok {
		s.goName = name
	} else {
		s.goName = s.c.GetName(s.c.InitialCap(s.apiName))
	}

	if s.Type().IsMap() {
		s.goReturnType = s.GoName()
	} else {
		s.goReturnType = "*" + s.GoName()
	}

	if s.Type().IsStruct() {
		pl := []*Property{}
		propMap := make(map[string]interface{})
		for k, v := range s.m.Properties {
			propMap[k] = v
		}
		for _, name := range s.c.SortedKeys(propMap) {
			m := s.m.Properties[name]
			pl = append(pl, &Property{
				s:       s,
				m:       m,
				apiName: name,
			})
		}

		s.properties = pl
	}

	return s
}

func (s *Schema) Type() *Type {
	return s.typ
}

func (s *Schema) GoName() string {
	return s.goName
}

func (s *Schema) GoReturnType() string {
	return s.goReturnType
}

func (s *Schema) GetProperties() []*Property {
	return s.properties
}

func (s *Schema) populateSubSchemas() (outerr error) {
	defer func() {
		r := recover()
		if r == nil {
			return
		}
		outerr = fmt.Errorf("%v", r)
	}()

	addSubStruct := func(subApiName string, t *Type) {
		if s.c.Schemas[subApiName] != nil {
			panic("dup schema apiName: " + subApiName)
		}
		subm := t.m
		subs:=NewSchema(s.c,subm,subApiName,t)
		s.c.Schemas[subApiName] = subs
		err := subs.populateSubSchemas()
		if err != nil {
			s.c.Panicf("in sub-struct %q: %v", subApiName, err)
		}
	}

	if s.Type().IsStruct() {
		for _, p := range s.GetProperties() {
			if p.Type().IsSimple() || p.Type().IsMap() {
				continue
			}
			if at, ok := p.Type().ArrayType(); ok {
				if at.IsSimple() || at.IsReference() {
					continue
				}
				subApiName := fmt.Sprintf("%s.%s", s.apiName, p.apiName)
				if at.IsStruct() {
					addSubStruct(subApiName, at) // was p.Type()?
					continue
				}
				if _, ok := at.ArrayType(); ok {
					addSubStruct(subApiName, at)
					continue
				}
				s.c.Panicf("Unknown property array type for %q: %s", subApiName, at)
				continue
			}
			subApiName := fmt.Sprintf("%s.%s", s.apiName, p.apiName)
			if p.Type().IsStruct() {
				addSubStruct(subApiName, p.Type())
				continue
			}
			if p.Type().IsReference() {
				continue
			}
			s.c.Panicf("Unknown type for %q: %s", subApiName, p.Type())
		}
		return
	}

	if at, ok := s.Type().ArrayType(); ok {
		if at.IsSimple() || at.IsReference() {
			return
		}
		subApiName := fmt.Sprintf("%s.Item", s.apiName)

		if at.IsStruct() {
			addSubStruct(subApiName, at)
			return
		}
		if at, ok := at.ArrayType(); ok {
			if at.IsSimple() || at.IsReference() {
				return
			}
			addSubStruct(subApiName, at)
			return
		}
		s.c.Panicf("Unknown array type for %q: %s", subApiName, at)
		return
	}

	if s.Type().IsSimple() || s.Type().IsReference() {
		return
	}

	fmt.Fprintf(os.Stderr, "in populateSubSchemas, schema is: %s", s.c.PrettyJSON(s.m))
	s.c.Panicf("populateSubSchemas: unsupported type for schema %q", s.apiName)
	panic("unreachable")
}

func (s *Schema) WriteSchemaCode() {
	if s.Type().IsAny() {
		s.c.Pn("\ntype %s interface{}", s.GoName())
		return
	}
	if s.Type().IsStruct() && !s.Type().IsMap() {
		s.writeSchemaStruct()
		return
	}

	if _, ok := s.Type().ArrayType(); ok {
		log.Printf("TODO writeSchemaCode for arrays for %s", s.GoName())
		return
	}

	if destSchema, ok := s.Type().ReferenceSchema(); ok {
		// Convert it to a struct using embedding.
		s.c.Pn("\ntype %s struct {", s.GoName())
		s.c.Pn(" %s", destSchema.GoName())
		s.c.Pn("}")
		return
	}

	if s.Type().IsSimple() {
		apitype := s.m.Type
		typ := s.c.MustSimpleTypeConvert(apitype, s.m.Format)
		s.c.Pn("\ntype %s %s", s.GoName(), typ)
		return
	}

	if s.Type().IsMap() {
		return
	}

	fmt.Fprintf(os.Stderr, "in writeSchemaCode, schema is: %s", s.c.PrettyJSON(s.m))
	s.c.Panicf("writeSchemaCode: unsupported type for schema %q", s.apiName)
}

func (s *Schema) Description() string {
	return s.m.Description
}

func (s *Schema) writeSchemaStruct() {
	//if v := jobj(s.m, "variant"); v != nil {//todo
	//	s.writeVariant(api, v)
	//	return
	//}
	s.c.P("\n")
	des := s.Description()
	if des != "" {
		s.c.P("%s", s.c.AsComment("", fmt.Sprintf("%s: %s", s.GoName(), des)))
	}
	s.c.Pn("type %s struct {", s.GoName())

	np := new(namePool)
	if s.isResponseType() {
		np.Get("ServerResponse") // reserve the name
	}

	if s.isRequestType() {
		all_args := make(map[string]*Argument)
		for _, meth := range s.c.APIMethods {
			args := meth.NewArguments()
			for _, arg := range args.m {
				if arg.location == "path" && meth.GetRequestType() == ("*"+s.GoName()) {
					all_args[arg.apiname] = arg
				}
			}
		}

		for _, res := range s.c.Resources {
			get_resource_methods_request_args(res, s.GoName(), all_args)
		}

		for _, arg := range all_args {
			exsit := false
			for _, p := range s.GetProperties() {
				pname := p.GoName()
				if pname == s.c.InitialCap(arg.goname) {
					exsit = true
				}
			}
			if !exsit {
				s.c.Pn(s.c.InitialCap(arg.goname) + " " + arg.gotype + " `json:\"" + arg.apiname + "\"`")
			}
		}
	}

	firstFieldName := "" // used to store a struct field name for use in documentation.
	for i, p := range s.GetProperties() {
		if i > 0 {
			s.c.P("\n")
		}

		pname := np.Get(p.GoName())
		if pname[0] == '@' {
			continue
		}

		des := p.Description()
		if des != "" {
			s.c.P("%s", s.c.AsComment("\t", fmt.Sprintf("%s: %s", pname, des)))
		}
		s.c.AddFieldValueComments(s.c.P, p, "\t", des != "")

		var extraOpt string
		if p.Type().IsIntAsString() {
			extraOpt += ",string"
		}

		typ := p.Type().AsGo()

		s.c.Pn(" %s %s `json:\"%s,omitempty%s\"`", pname, typ, p.APIName(), extraOpt)
		if firstFieldName == "" {
			firstFieldName = pname
		}
	}

	if s.isResponseType() {
		if firstFieldName != "" {
			s.c.P("\n")
		}
		s.c.P("%s", s.c.AsComment("\t", "ServerResponse contains the HTTP response code and headers from the server."))
		s.c.P(" restful.ServerResponse `json:\"-\"`")
	}

	if firstFieldName == "" {
		// There were no fields in the struct, so there is no point
		// adding any custom JSON marshaling code.
		s.c.Pn("}")
		return
	}

	s.c.Pn("}")
	s.c.Pn("")

	return
}

// writeSchemaMarshal writes a custom MarshalJSON function for s, which allows
// fields to be explicitly transmitted by listing them in the field identified
// by forceSendFieldName.
func (s *Schema) writeSchemaMarshal(forceSendFieldName string) {
	s.c.Pn("func (s *%s) MarshalJSON() ([]byte, error) {", s.GoName())
	s.c.Pn("\ttype noMethod %s", s.GoName())
	// pass schema as methodless type to prevent subsequent calls to MarshalJSON from recursing indefinitely.
	s.c.Pn("\traw := noMethod(*s)")
	s.c.Pn("\treturn generate.MarshalJSON(raw, s.%s)", forceSendFieldName)
	s.c.Pn("}")
}

func (s *Schema) writeSchemaUnmarshal(forceSendFieldName string) {
	s.c.Pn("func (s *%s) UnmarshalJSON(data []byte) error {", s.GoName())
	s.c.Pn("\ttype noMethod %s", s.GoName())
	// pass schema as methodless type to prevent subsequent calls to MarshalJSON from recursing indefinitely.
	s.c.Pn("\traw := noMethod(*s)")
	s.c.Pn("\treturn generate.UnmarshalJSON(data,raw, s.%s)", forceSendFieldName)
	s.c.Pn("}")
}

// isResponseType returns true for all types that are used as a response.
func (s *Schema) isResponseType() bool {
	return s.c.ResponseTypes["*"+s.goName]
}

func (s *Schema) isRequestType() bool {
	return s.c.RequestTypes["*"+s.goName]
}

func get_resource_methods_request_args(r *Resource, schemaName string, all_args map[string]*Argument) {
	for _, meth := range r.Methods {
		args := meth.NewArguments()
		for _, arg := range args.m {
			if arg.location == "path" && meth.GetRequestType() == ("*"+schemaName) {
				all_args[arg.apiname] = arg
			}
		}
	}

	for _, subResource := range r.resources {
		get_resource_methods_request_args(subResource, schemaName, all_args)
	}

	return
}
