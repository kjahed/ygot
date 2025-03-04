// Copyright 2017 Google Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package ygot contains helper methods for dealing with structs that represent
// a YANG schema. Particularly, it takes structs that represent a YANG schema -
// generated by ygen:
//	- Provides helper functions which simplify their usage such as functions
//	  to return pointers to a type.
//	- Renders structs to other output formats such as JSON, or gNMI
//	  notifications.
package ygot

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"

	"github.com/google/go-cmp/cmp"
	"github.com/openconfig/ygot/util"
)

const (
	// indentString represents the default indentation string used for
	// JSON. Three spaces are used based on the legacy use of EmitJSON.
	indentString string = "   "
)

// structTagToLibPaths takes an input struct field as a reflect.Type, and determines
// the set of validation library paths that it maps to. Returns the paths as a slice of
// empty interface slices, or an error.
func structTagToLibPaths(f reflect.StructField, parentPath *gnmiPath, preferShadowPath bool) ([]*gnmiPath, error) {
	if !parentPath.isValid() {
		return nil, fmt.Errorf("invalid path format in parentPath (%v, %v)", parentPath.stringSlicePath == nil, parentPath.pathElemPath == nil)
	}

	var pathAnnotation string
	var ok bool
	if preferShadowPath {
		pathAnnotation, ok = f.Tag.Lookup("shadow-path")
	}
	if !ok {
		if pathAnnotation, ok = f.Tag.Lookup("path"); !ok {
			return nil, fmt.Errorf("field did not specify a path")
		}
	}

	var mapPaths []*gnmiPath
	tagPaths := strings.Split(pathAnnotation, "|")
	for _, p := range tagPaths {
		// Make a copy of the existing parent path so we can append to it without
		// modifying it for future paths.
		ePath := parentPath.Copy()

		for _, pp := range strings.Split(p, "/") {
			// Handle empty path tags.
			if pp == "" {
				continue
			}
			ePath.AppendName(pp)
		}

		if len(p) > 0 && p[0] == '/' {
			ePath.isAbsolute = true
		}

		mapPaths = append(mapPaths, ePath)
	}
	return mapPaths, nil
}

// structTagToLibModules takes an input struct field as a reflect.Type, and
// extracts the set of module names in the module or shadow-module struct tag
// of the field. Returns the module names as a slice of gnmiPaths, or an error.
// If the field were generated correctly, then these module names should have
// a 1:1 correspondence to the path names in the path tag, and denotes the
// module to which each path element belongs (using YANG's XML namespace
// rules).
func structTagToLibModules(f reflect.StructField, preferShadowPath bool) ([]*gnmiPath, error) {
	var moduleAnnotation string
	var ok bool
	if preferShadowPath {
		moduleAnnotation, ok = f.Tag.Lookup("shadow-module")
	}
	if !ok {
		if moduleAnnotation, ok = f.Tag.Lookup("module"); !ok {
			return nil, nil
		}
	}

	var mapModules []*gnmiPath
	for _, m := range strings.Split(moduleAnnotation, "|") {
		eModule := newStringSliceGNMIPath(nil)
		for _, mm := range strings.Split(m, "/") {
			// Handle empty module tags.
			if mm == "" {
				continue
			}
			eModule.AppendName(mm)
		}

		switch {
		case len(m) == 0:
			return nil, fmt.Errorf("module tag must not have an empty path: %s", moduleAnnotation)
		case m[0] == '/':
			eModule.isAbsolute = true
		}

		mapModules = append(mapModules, eModule)
	}
	return mapModules, nil
}

// EnumName returns the string name of an input GoEnum e. If the enumeration is
// unset, the name returned is an empty string, otherwise it is the name defined
// within the YANG schema. Non-zero out-of-range values and unrecognized enums
// will produce an error.
func EnumName(e GoEnum) (string, error) {
	name, _, err := enumFieldToString(reflect.ValueOf(e), false)
	return name, err
}

// enumFieldToString takes an input reflect.Value, which is type asserted to
// be a GoEnum, and resolves the string name corresponding to the value within
// the YANG schema. Returns the string name of the enum, a bool indicating
// whether the value was set, or an error. The prependModuleNameIref specifies whether
// the defining module name should be appended to the enumerated value's name in
// the form "module:name", as per the encoding rules in RFC7951.
func enumFieldToString(field reflect.Value, prependModuleNameIref bool) (string, bool, error) {
	// Generated structs can only have fields that are not pointers when they are enumerated
	// values, since these values have an UNSET value that allows us to determine when they
	// are not explicitly set by the user.
	// We check whether this is an enum field by checking whether the type implements the
	// GoEnum interface.
	enumVal, isEnum := field.Interface().(GoEnum)
	if !isEnum {
		return "", false, fmt.Errorf("supplied value was not a valid GoEnum: %v", field.Type())
	}

	e := reflect.ValueOf(enumVal)

	if e.Int() == 0 {
		// Enumerations are always derived int64 types, which have a default of
		// 0. The generated enumeration's _UNSET value is always zero, so we can
		// use this to determine that the enumeration was not explicitly set by
		// the user and skip mapping this leaf into the schema.
		return "", false, nil
	}

	// ΛMap returns a map that is keyed based on the name of the enumeration's Go type,
	// which provides a map between the integer values of the enumeration and the strings.
	// The ygen library expects input of the string names of the enumeration, so extract this.
	lookup, ok := enumVal.ΛMap()[e.Type().Name()]
	if !ok {
		return "", false, fmt.Errorf("cannot map enumerated value as type %s was unknown", field.Type().Name())
	}

	def, ok := lookup[e.Int()]
	if !ok {
		return "", false, fmt.Errorf("cannot map enumerated value as type %s has unknown value %d", field.Type().Name(), enumVal)
	}

	n := def.Name
	if prependModuleNameIref && def.DefiningModule != "" {
		n = fmt.Sprintf("%s:%s", def.DefiningModule, def.Name)
	}
	return n, true, nil
}

// EnumLogString uses the EnumDefinition map of the given enum, an input
// int64 val, and the input type name of the enum to output a log-friendly string.
// If val is a valid enum value, then the defined YANG string corresponding to
// the enum value is returned; otherwise, an out-of-range error string is returned.
func EnumLogString(e GoEnum, val int64, enumTypeName string) string {
	enumDef, ok := e.ΛMap()[enumTypeName][val]
	if !ok {
		return fmt.Sprintf("out-of-range %s enum value: %v", enumTypeName, val)
	}
	return enumDef.Name
}

// BuildEmptyTree initialises the YANG tree starting at the root GoStruct
// provided. This allows the YANG container hierarchy (i.e., any structs within
// the tree) to be pre-initialised rather than requiring the user to initialise
// each as it is required. Given that some trees may be large, then some
// caution should be exercised in initialising an entire tree. If struct pointer
// fields are non-nil, they are considered initialised, and are skipped.
func BuildEmptyTree(s GoStruct) {
	initialiseTree(reflect.ValueOf(s).Elem().Type(), reflect.ValueOf(s).Elem())
}

// initialiseTree takes an input data item's reflect.Value and reflect.Type for
// a particular GoStruct, and initialises the nested structs that are within it.
func initialiseTree(t reflect.Type, v reflect.Value) {
	for i := 0; i < v.NumField(); i++ {
		fVal := v.Field(i)
		fType := t.Field(i)

		if util.IsTypeStructPtr(fType.Type) {
			// Only initialise nested struct pointers, since all struct fields within
			// a GoStruct are expected to be pointers, and we do not want to initialise
			// non-struct values. If the struct pointer is not nil, it is skipped.
			if !fVal.IsNil() {
				continue
			}

			pVal := reflect.New(fType.Type.Elem())
			initialiseTree(pVal.Elem().Type(), pVal.Elem())
			fVal.Set(pVal)
		}
	}
}

// PruneEmptyBranches removes branches that have no populated children from the
// GoStruct s in-place. This allows a YANG container hierarchy that has been
// initialised with BuildEmptyTree to have those branches that were not populated
// removed from the tree. All subtrees rooted at the supplied GoStruct are traversed
// and any encountered GoStruct pointer fields are removed if they equate to
// the zero value (i.e. are unpopulated).
func PruneEmptyBranches(s GoStruct) {
	v := reflect.ValueOf(s).Elem()
	pruneBranchesInternal(v.Type(), v)
}

// pruneBranchesInternal implements the logic to remove empty branches from the
// supplied reflect.Type, reflect.Value which must represent a GoStruct. An empty
// tree is defined to be a struct that is equal to its zero value. Only struct
// pointer fields are examined, since these are subtrees within the generated GoStruct
// types. It returns a bool which indicates whether all fields of the struct were
// removed.
func pruneBranchesInternal(t reflect.Type, v reflect.Value) bool {
	// Track whether all fields of the GoStruct are nil, such that it can
	// be returned to the caller. This allows parents that have all empty
	// children to be removed. This is required because BuildEmptyTree will
	// propagate to all branches.
	allChildrenPruned := true
	for i := 0; i < v.NumField(); i++ {
		fVal := v.Field(i)
		fType := t.Field(i)
		if util.IsTypeStructPtr(fType.Type) {
			// Create an empty version of the struct that is within the struct pointer.
			// We can safely call Elem() here since we verified above that this type
			// is a struct pointer.
			zVal := reflect.Zero(fType.Type.Elem())

			switch {
			case fVal.IsNil():
				// Ensure that if the field value was actually nil, we skip over this
				// field since its already nil.
				continue
			case reflect.DeepEqual(zVal.Interface(), fVal.Elem().Interface()):
				// In the case that the zero value's interface is the same as the
				// dereferenced field value's nil value, then we set it to the zero value
				// of the field type. The fType contains a pointer to the struct, so
				// reflect.Zero returns nil here.
				fVal.Set(reflect.Zero(fType.Type))
				continue
			default:
				// If this wasn't an empty struct then we need to recurse to remove
				// any nil children of this struct.
				sv := fVal.Elem()
				childPruned := pruneBranchesInternal(sv.Type(), sv)
				if childPruned {
					// If all fields of the downstream branches are nil, then
					// also prune this field.
					fVal.Set(reflect.Zero(fType.Type))
				} else {
					allChildrenPruned = false
				}
			}
			continue
		}

		// If the struct field wasn't a struct pointer, then we need to check whether it
		// is the nil value of its type.
		switch {
		case util.IsTypeSlice(fType.Type):
			if (fVal.Len() != 0) && allChildrenPruned {
				allChildrenPruned = false
			}
		case util.IsTypeMap(fType.Type):
			if fVal.Len() != 0 && allChildrenPruned {
				allChildrenPruned = false
			}

			// Recurse into maps where the children may have already been initialised.
			for _, k := range fVal.MapKeys() {
				mi := fVal.MapIndex(k)
				if !util.IsValueStructPtr(mi) {
					continue
				}
				sv := mi.Elem()
				// We can discard the pruneBranchesInternal return value, since we
				// know that this map field has len > 0, and therefore cannot be
				// pruned.
				_ = pruneBranchesInternal(sv.Type(), sv)
			}
		default:
			// Handle the case of a non-map/slice/struct pointer field.
			v := fVal
			t := fType.Type
			if fType.Type.Kind() == reflect.Ptr {
				if !v.IsNil() {
					allChildrenPruned = false
					continue
				}
				// Dereference the pointer to allow a zero check.
				v = v.Elem()
				t = t.Elem()
			}
			if v.IsValid() && !reflect.DeepEqual(reflect.Zero(t).Interface(), v.Interface()) {
				allChildrenPruned = false
			}
		}

	}
	return allChildrenPruned
}

// InitContainer initialises the container cname of the GoStruct s, it can be
// used to initialise an arbitrary named child container within a YANG
// structure in a generic manner. This allows the caller to generically
// initialise a sub-element of the YANG tree without needing to have specific
// handling code.
func InitContainer(s GoStruct, cname string) error {
	f := reflect.ValueOf(s).Elem().FieldByName(cname)
	if !f.IsValid() {
		return fmt.Errorf("invalid container %s as child of %v", cname, s)
	}
	t := f.Type()

	if n := reflect.New(t.Elem()); n.Elem().Type().Kind() == reflect.Struct {
		f.Set(n)
		return nil
	}

	return fmt.Errorf("field %s was not a struct to initialise", cname)
}

// binaryBase64 takes an input byte slice and returns it as a base64
// encoded string.
func binaryBase64(i []byte) string {
	var b bytes.Buffer
	encoder := base64.NewEncoder(base64.StdEncoding, &b)
	encoder.Write(i)
	encoder.Close()
	return b.String()
}

// JSONFormat is an enumerated integer value indicating the JSON format.
type JSONFormat int

const (
	// Internal is the custom JSON format that is output by the validation library, and
	// by pyangbind. It is loosely specified - but is the default used by generator developers.
	Internal JSONFormat = iota
	// RFC7951 is JSON that conforms to RFC7951.
	RFC7951
)

// EmitJSONConfig specifies the how JSON should be created by the EmitJSON function.
type EmitJSONConfig struct {
	// Format specifies the JSON format that should be output by the EmitJSON
	// function - using the enumerated JSONType function. By default, internal
	// format JSON will be produced.
	Format JSONFormat
	// RFC7951Config specifies the configuration options for RFC7951 JSON. Only
	// valid if Format is RFC7951.
	RFC7951Config *RFC7951JSONConfig
	// Indent is the string used for indentation within the JSON output. The
	// default value is three spaces.
	Indent string
	// EscapeHTML determines whether certain characters will be escaped
	// in the marshalled JSON for safety in HTML embedding. See
	// https://pkg.go.dev/encoding/json#Encoder.SetEscapeHTML.
	EscapeHTML bool
	// SkipValidation specifies whether the GoStruct supplied to EmitJSON should
	// be validated before emitting its content. Validation is skipped when it
	// is set to true.
	SkipValidation bool
	// ValidationOpts is the set of options that should be used to determine how
	// the schema should be validated. This allows fine-grained control of particular
	// validation rules in the case that a partially populated data instance is
	// to be emitted.
	ValidationOpts []ValidationOption
}

// EmitJSON takes an input GoStruct (produced by ygen with validation enabled)
// and serialises it to a JSON string. By default, produces the Internal format JSON.
func EmitJSON(gs GoStruct, opts *EmitJSONConfig) (string, error) {
	var (
		vopts          []ValidationOption
		skipValidation bool
	)

	if opts != nil {
		vopts = opts.ValidationOpts
		skipValidation = opts.SkipValidation
	}

	s, ok := gs.(validatedGoStruct)
	if !ok {
		return "", fmt.Errorf("input GoStruct does not have ΛValidate() method")
	}

	if !skipValidation {
		if err := s.ΛValidate(vopts...); err != nil {
			return "", fmt.Errorf("validation err: %v", err)
		}
	}

	v, err := makeJSON(s, opts)
	if err != nil {
		return "", err
	}

	sb := &strings.Builder{}
	enc := json.NewEncoder(sb)
	indent := indentString
	enc.SetEscapeHTML(false)
	if opts != nil {
		enc.SetEscapeHTML(opts.EscapeHTML)

		if opts.Indent != "" {
			indent = opts.Indent
		}
	}
	enc.SetIndent("", indent)

	if err := enc.Encode(v); err != nil {
		return "", fmt.Errorf("JSON marshalling error: %v", err)
	}

	// Exclude the last newline character:
	// https://pkg.go.dev/encoding/json#Encoder.Encode
	return sb.String()[:sb.Len()-1], nil
}

// makeJSON renders the GoStruct s to map[string]interface{} according to the
// JSON format specified. By default makeJSON returns internal format JSON.
func makeJSON(s GoStruct, opts *EmitJSONConfig) (map[string]interface{}, error) {
	f := Internal
	if opts != nil {
		f = opts.Format
	}

	var v map[string]interface{}
	var err error
	switch f {
	case Internal:
		if v, err = ConstructInternalJSON(s); err != nil {
			return nil, fmt.Errorf("ConstructInternalJSON error: %v", err)
		}
	case RFC7951:
		var c *RFC7951JSONConfig
		if opts != nil {
			c = opts.RFC7951Config
		}
		if v, err = ConstructIETFJSON(s, c); err != nil {
			return nil, fmt.Errorf("ConstructIETFJSON error: %v", err)
		}
	}
	return v, nil
}

// MergeStructJSON marshals the GoStruct ns to JSON according to the configuration, and
// merges it with the existing JSON provided as a map[string]interface{}. The merged
// JSON output is returned.
//
// To create valid JSON-serialised YANG, it is expected that the existing JSON is in
// the same format as is specified in the options. Where there are overlapping tree
// elements in the serialised struct they are merged where possible.
func MergeStructJSON(ns GoStruct, ej map[string]interface{}, opts *EmitJSONConfig) (map[string]interface{}, error) {
	j, err := makeJSON(ns, opts)
	if err != nil {
		return nil, err
	}

	nj, err := MergeJSON(ej, j)
	if err != nil {
		return nil, err
	}
	return nj, nil
}

// MergeJSON takes two input maps, and merges them into a single map.
func MergeJSON(a, b map[string]interface{}) (map[string]interface{}, error) {
	o := map[string]interface{}{}

	// Copy map a into the output.
	for k, v := range a {
		o[k] = v
	}

	for k, v := range b {
		if _, ok := o[k]; !ok {
			// Simple case, where the branch in b does not exist in
			// a, so we can simply add the subtree.
			o[k] = v
			continue
		}

		src, sok := o[k].(map[string]interface{})
		dst, dok := v.(map[string]interface{})
		if sok && dok {
			// The key exists in both a and b, and is a map[string]interface{}
			// in both, such that it can be merged as the subtree.
			var err error
			o[k], err = MergeJSON(src, dst)
			if err != nil {
				return nil, err
			}
			continue
		}

		ssrc, sok := o[k].([]interface{})
		sdst, dok := v.([]interface{})
		if sok && dok {
			// The key exists in both a and b, and is a slice
			// such that we can concat the two slices.
			o[k] = append(ssrc, sdst...)
			continue
		}

		return nil, fmt.Errorf("%s is not a mergable JSON type in tree, a: %T, b: %T", k, o[k], v)
	}

	return o, nil
}

// MergeOpt is an interface that is implemented by the options to the
// MergeStructs and MergeStructInto functions.
type MergeOpt interface {
	// IsMergeOpt is a marker method for each MergeOpt.
	IsMergeOpt()
}

// MergeOverwriteExistingFields is a MergeOpt that allows control of the merge behaviour
// of MergeStructs and MergeStructInto functions.
//
// When used, fields that are populated in the destination struct will be overwritten
// by values that are populated in the source struct. If the field is unpopulated
// in the source struct, the value in the destination struct will not be modified.
type MergeOverwriteExistingFields struct{}

// IsMergeOpt marks MergeStructOpt as a MergeOpt.
func (*MergeOverwriteExistingFields) IsMergeOpt() {}

// MergeEmptyMaps is a MergeOpt that allows control of the merge behaviour
// of MergeStructs and MergeStructInto functions.
//
// When used, if both the destination struct and the source struct has an empty
// map field, but it is non-nil in either one, then that map field in the
// destination will always be populated with an empty, non-nil map value.
//
// NOTE: Since YANG doesn't distinguish between a nil map and an empty map,
// please consider another approach before using this option.
type MergeEmptyMaps struct{}

// IsMergeOpt marks MergeEmptyMaps as a MergeOpt.
func (*MergeEmptyMaps) IsMergeOpt() {}

// MergeStructs takes two input GoStruct and merges their contents,
// returning a new GoStruct. If the input structs a and b are of
// different types, an error is returned.
//
// Where two structs contain maps or slices that are populated in both a and b,
// merge is skipped if their contents are equal, and their contents are merged
// if unequal; however, an error is returned for slices if their elements are
// overlapping but not equal. If a leaf is populated in both a and b, an error
// is returned if the value of the leaf is not equal.
func MergeStructs(a, b GoStruct, opts ...MergeOpt) (GoStruct, error) {
	if reflect.TypeOf(a) != reflect.TypeOf(b) {
		return nil, fmt.Errorf("cannot merge structs that are not of matching types, %T != %T", a, b)
	}

	dst, err := deepCopy(a, mergeEmptyMapsEnabled(opts))
	if err != nil {
		return nil, err
	}

	if err := MergeStructInto(dst, b, opts...); err != nil {
		return nil, fmt.Errorf("error merging b to new struct: %v", err)
	}

	return dst, nil
}

// MergeStructInto takes the provided input GoStruct and merges the
// contents from src into dst. Unlike MergeStructs, the supplied dst is mutated.
//
// The merge semantics are the same as those for MergeStructs.
func MergeStructInto(dst, src GoStruct, opts ...MergeOpt) error {
	if reflect.TypeOf(dst) != reflect.TypeOf(src) {
		return fmt.Errorf("cannot merge structs that are not of matching types, %T != %T", dst, src)
	}

	return copyStruct(reflect.ValueOf(dst).Elem(), reflect.ValueOf(src).Elem(), opts...)
}

// DeepCopy returns a deep copy of the supplied GoStruct. A new copy
// of the GoStruct is created, along with any underlying values.
func DeepCopy(s GoStruct) (GoStruct, error) {
	return deepCopy(s, false)
}

// deepCopy returns a deep copy of the supplied GoStruct. A new copy
// of the GoStruct is created, along with any underlying values.
// If keepEmptyMaps is true, then empty but non-nil maps are kept in the deep
// copy.
func deepCopy(s GoStruct, keepEmptyMaps bool) (GoStruct, error) {
	if util.IsNilOrInvalidValue(reflect.ValueOf(s)) {
		return nil, fmt.Errorf("invalid input to DeepCopy, got nil value: %v", s)
	}
	n := reflect.New(reflect.TypeOf(s).Elem())
	var opts []MergeOpt
	if keepEmptyMaps {
		opts = append(opts, &MergeEmptyMaps{})
	}
	if err := copyStruct(n.Elem(), reflect.ValueOf(s).Elem(), opts...); err != nil {
		return nil, fmt.Errorf("cannot DeepCopy struct: %v", err)
	}
	return n.Interface().(GoStruct), nil
}

// fieldOverwriteEnabled returns true if MergeOverwriteExistingFields
// is present in the slice of MergeOpt.
func fieldOverwriteEnabled(opts []MergeOpt) bool {
	for _, o := range opts {
		switch o.(type) {
		case *MergeOverwriteExistingFields:
			return true
		}
	}
	return false
}

// mergeEmptyMapsEnabled returns true if MergeEmptyMaps
// is present in the slice of MergeOpt.
func mergeEmptyMapsEnabled(opts []MergeOpt) bool {
	for _, o := range opts {
		switch o.(type) {
		case *MergeEmptyMaps:
			return true
		}
	}
	return false
}

// copyStruct copies the fields of srcVal into the dstVal struct in-place.
func copyStruct(dstVal, srcVal reflect.Value, opts ...MergeOpt) error {
	if srcVal.Type() != dstVal.Type() {
		return fmt.Errorf("cannot copy %s to %s", srcVal.Type().Name(), dstVal.Type().Name())
	}

	if !util.IsValueStruct(dstVal) || !util.IsValueStruct(srcVal) {
		return fmt.Errorf("cannot handle non-struct types, src: %v, dst: %v", srcVal.Type().Kind(), dstVal.Type().Kind())
	}

	for i := 0; i < srcVal.NumField(); i++ {
		srcField := srcVal.Field(i)
		dstField := dstVal.Field(i)

		switch srcField.Kind() {
		case reflect.Ptr:
			if err := copyPtrField(dstField, srcField, opts...); err != nil {
				return err
			}
		case reflect.Interface:
			if err := copyInterfaceField(dstField, srcField, opts...); err != nil {
				return err
			}
		case reflect.Map:
			if err := copyMapField(dstField, srcField, opts...); err != nil {
				return err
			}
		case reflect.Slice:
			if err := copySliceField(dstField, srcField, opts...); err != nil {
				return err
			}
		case reflect.Int64:
			// In the case of an int64 field, which represents a YANG enumeration
			// we should only set the value in the destination if it is not set
			// to the default value in the source.
			vSrc, vDst := srcField.Int(), dstField.Int()
			switch {
			case vSrc != 0 && vDst != 0 && vSrc != vDst:
				if !fieldOverwriteEnabled(opts) {
					return fmt.Errorf("destination and source values were set when merging enum field, dst: %d, src: %d", vSrc, vDst)
				}
				dstField.Set(srcField)
			case vSrc != 0 && vDst == 0:
				dstField.Set(srcField)
			}
		default:
			dstField.Set(srcField)
		}
	}
	return nil
}

// copyPtrField copies srcField to dstField. srcField and dstField must be
// reflect.Value structs which represent pointers. If the source and destination
// are struct pointers, then their contents are merged. If the source and
// destination are non-struct pointers, values are not merged and an error
// is returned. If the source and destination both have a pointer field, which is
// populated then an error is returned unless the value of the field is
// equal in both structs.
func copyPtrField(dstField, srcField reflect.Value, opts ...MergeOpt) error {

	if util.IsNilOrInvalidValue(srcField) {
		return nil
	}

	if !util.IsValuePtr(srcField) {
		return fmt.Errorf("received non-ptr type: %v", srcField.Kind())
	}

	// Check for struct ptr, or ptr to avoid panic.
	if util.IsValueStructPtr(srcField) {
		var d reflect.Value

		// If the destination value is non-nil, then we maintain its contents
		// this ensures that we maintain the non-overlapping contents in the
		// struct that is being copied to.
		if util.IsNilOrInvalidValue(dstField) {
			d = reflect.New(srcField.Type().Elem())
		} else {
			d = dstField
		}

		if err := copyStruct(d.Elem(), srcField.Elem(), opts...); err != nil {
			return err
		}
		dstField.Set(d)
		return nil
	}

	if !util.IsNilOrInvalidValue(dstField) {
		s, d := srcField.Elem().Interface(), dstField.Elem().Interface()
		if diff := cmp.Diff(s, d); !fieldOverwriteEnabled(opts) && diff != "" {
			return fmt.Errorf("destination value was set, but was not equal to source value when merging ptr field, (-src, +dst):\n%s", diff)
		}
	}

	p := reflect.New(srcField.Type().Elem())
	p.Elem().Set(srcField.Elem())
	dstField.Set(p)
	return nil
}

// copyInterfaceField copies srcField into dstField. Both srcField and dstField
// are reflect.Value structs which contain an interface value.
func copyInterfaceField(dstField, srcField reflect.Value, opts ...MergeOpt) error {
	if util.IsNilOrInvalidValue(srcField) {
		return nil
	}

	if !util.IsValueInterface(srcField) {
		return fmt.Errorf("non-interface type received: %T", srcField.Interface())
	}

	_, isGoEnum := srcField.Elem().Interface().(GoEnum)
	switch {
	case util.IsValueStructPtr(srcField.Elem()):
		s := srcField.Elem().Elem() // Dereference src to a struct.
		if !util.IsNilOrInvalidValue(dstField) {
			dV := dstField.Elem().Elem() // Dereference dst to a struct.
			if diff := cmp.Diff(s.Interface(), dV.Interface()); !fieldOverwriteEnabled(opts) && diff != "" {
				return fmt.Errorf("interface field was set in both src and dst and was not equal, (-src, +dst):\n%s", diff)
			}
		}

		d := reflect.New(s.Type())
		if err := copyStruct(d.Elem(), s, opts...); err != nil {
			return err
		}
		dstField.Set(d)
		return nil
	case srcField.Elem().Kind() == reflect.Slice && srcField.Elem().Type().Name() == BinaryTypeName:
		if !util.IsNilOrInvalidValue(dstField) {
			s, d := srcField.Interface(), dstField.Interface()
			if diff := cmp.Diff(s, d); !fieldOverwriteEnabled(opts) && diff != "" {
				return fmt.Errorf("interface field was set in both src and dst and was not equal, (-src, +dst):\n%s", diff)
			}
		}

		srcVal := srcField.Elem()
		ns := reflect.Zero(srcVal.Type())
		for i := 0; i < srcVal.Len(); i++ {
			ns = reflect.Append(ns, srcVal.Index(i))
		}
		dstField.Set(ns)
		return nil
	case util.IsValueScalar(srcField.Elem()) && (isGoEnum || unionSingletonUnderlyingTypes[srcField.Elem().Type().Name()] != nil):
		if !util.IsNilOrInvalidValue(dstField) {
			s, d := srcField.Interface(), dstField.Interface()
			if diff := cmp.Diff(s, d); !fieldOverwriteEnabled(opts) && diff != "" {
				return fmt.Errorf("interface field was set in both src and dst and was not equal, (-src, +dst):\n%s", diff)
			}
		}
		dstField.Set(srcField)
		return nil
	}
	return fmt.Errorf("invalid interface type received: %T", srcField.Interface())
}

// copyMapField copies srcField into dstField. Both srcField and dstField are
// reflect.Value structs which contain a map value. If both srcField and dstField
// are populated, and have non-overlapping keys, they are merged. If the same
// key is populated in srcField and dstField, their contents are merged if they
// do not overlap, otherwise an error is returned.
func copyMapField(dstField, srcField reflect.Value, opts ...MergeOpt) error {
	if !util.IsValueMap(srcField) {
		return fmt.Errorf("received a non-map type in src map field: %v", srcField.Kind())
	}

	if !util.IsValueMap(dstField) {
		return fmt.Errorf("received a non-map type in dst map field: %v", dstField.Kind())
	}

	// Skip cases where there are empty maps in both src and dst.
	// Exception: user wants an empty map to be merged as well.
	if srcField.Len() == 0 && dstField.Len() == 0 {
		if !mergeEmptyMapsEnabled(opts) || srcField.IsNil() {
			return nil
		}
	}

	m, err := validateMap(srcField, dstField)
	if err != nil {
		return err
	}

	if dstField.Len() == 0 {
		dstField.Set(reflect.MakeMapWithSize(reflect.MapOf(m.key, m.value), srcField.Len()))
	}

	dstKeys := map[interface{}]bool{}
	for _, k := range dstField.MapKeys() {
		dstKeys[k.Interface()] = true
	}

	for _, k := range srcField.MapKeys() {
		v := srcField.MapIndex(k)
		d := reflect.New(v.Elem().Type())
		if _, ok := dstKeys[k.Interface()]; ok {
			d = dstField.MapIndex(k)
		}
		if err := copyStruct(d.Elem(), v.Elem(), opts...); err != nil {
			return err
		}
		dstField.SetMapIndex(k, d)
	}
	return nil
}

// mapTypes provides a specification of a map.
type mapType struct {
	key   reflect.Type // key is the type of the key of the map.
	value reflect.Type // value is the type of the value of the map.
}

// validateMap checks the srcField and dstField reflect.Value structs
// to ensure that they are valid maps of struct pointers, and that their keys
// types are the same. It returns a specification of the map type if the maps
// match.
func validateMap(srcField, dstField reflect.Value) (*mapType, error) {
	if s := srcField.Kind(); s != reflect.Map {
		return nil, fmt.Errorf("invalid src field, was not a map, was: %v", s)
	}

	if d := dstField.Kind(); d != reflect.Map {
		return nil, fmt.Errorf("invalid dst field, was not a map, was: %v", d)
	}

	st, dt := srcField.Type(), dstField.Type()
	se, de := st.Elem(), dt.Elem()
	if se != de {
		return nil, fmt.Errorf("invalid maps, src and dst value types are different, %v != %v", se, de)
	}

	if !util.IsTypeStructPtr(se) || !util.IsTypeStructPtr(de) {
		return nil, fmt.Errorf("invalid maps, src or dst does not have a struct ptr element, src: %v, dst: %v", se.Kind(), de.Kind())
	}

	if sk, dk := st.Key(), dt.Key(); sk != dk {
		return nil, fmt.Errorf("invalid maps, src and dst key types are different, %v != %v", sk, dk)
	}

	return &mapType{key: st.Key(), value: st.Elem()}, nil
}

// copySliceField copies srcField into dstField. Both srcField and dstField
// must have a kind of reflect.Slice kind and contain pointers to structs. If
// the slice in dstField is populated an error is returned.
func copySliceField(dstField, srcField reflect.Value, opts ...MergeOpt) error {
	if dstField.Len() == 0 && srcField.Len() == 0 {
		return nil
	}

	if _, ok := srcField.Interface().([]Annotation); !ok {
		if reflect.DeepEqual(srcField.Interface(), dstField.Interface()) {
			return nil
		}

		unique, err := uniqueSlices(dstField, srcField)
		if err != nil {
			return fmt.Errorf("error checking src and dst for uniqueness, got: %v", err)
		}

		if !unique {
			// YANG lists and leaf-lists must be unique.
			return fmt.Errorf("source and destination lists must be unique, got src: %v, dst: %v", srcField, dstField)
		}
	}

	if !util.IsTypeStructPtr(srcField.Type().Elem()) {
		for i := 0; i < srcField.Len(); i++ {
			v := srcField.Index(i)
			dstField.Set(reflect.Append(dstField, v))
		}
		return nil
	}

	for i := 0; i < srcField.Len(); i++ {
		v := srcField.Index(i)
		d := reflect.New(v.Type().Elem())
		if err := copyStruct(d.Elem(), v.Elem(), opts...); err != nil {
			return err
		}
		dstField.Set(reflect.Append(dstField, v))
	}
	return nil
}

// uniqueSlices takes two reflect.Values which must represent slices, and determines
// whether a and b are disjoint. It returns true if the slices have unique
// members, and false if not.
func uniqueSlices(a, b reflect.Value) (bool, error) {
	if !util.IsValueSlice(a) || !util.IsValueSlice(b) {
		return false, fmt.Errorf("a and b must both be slices, got a: %v, b: %v", a.Type().Kind(), b.Type().Kind())
	}

	if a.Type().Elem() != b.Type().Elem() {
		return false, fmt.Errorf("a and b do not contain the same type, got a: %v, b: %v", a.Type().Elem().Kind(), b.Type().Elem().Kind())
	}

	for i := 0; i < a.Len(); i++ {
		for j := 0; j < b.Len(); j++ {
			if reflect.DeepEqual(a.Index(i).Interface(), b.Index(j).Interface()) {
				return false, nil
			}
		}
	}
	return true, nil
}
