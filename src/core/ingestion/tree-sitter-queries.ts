import { SupportedLanguages } from '../../config/supported-languages';

/* ── TypeScript / TSX ───────────────────────────────────── */

const tsQueries = `
(class_declaration
  name: (type_identifier) @name) @definition.class

(interface_declaration
  name: (type_identifier) @name) @definition.interface

(function_declaration
  name: (identifier) @name) @definition.function

(method_definition
  name: (property_identifier) @name) @definition.method

(lexical_declaration
  (variable_declarator
    name: (identifier) @name
    value: (arrow_function))) @definition.function

(lexical_declaration
  (variable_declarator
    name: (identifier) @name
    value: (function_expression))) @definition.function

(export_statement
  declaration: (lexical_declaration
    (variable_declarator
      name: (identifier) @name
      value: (arrow_function)))) @definition.function

(export_statement
  declaration: (lexical_declaration
    (variable_declarator
      name: (identifier) @name
      value: (function_expression)))) @definition.function

; Exported const/let with any value (non-function values captured as const)
(export_statement
  declaration: (lexical_declaration
    (variable_declarator
      name: (identifier) @name
      value: (_)))) @definition.const

; Top-level const/let at module scope
(program
  (lexical_declaration
    (variable_declarator
      name: (identifier) @name
      value: (_)))) @definition.const

(import_statement
  source: (string) @import.source) @import

(call_expression
  function: (identifier) @call.name) @call

(call_expression
  function: (member_expression
    property: (property_identifier) @call.name)) @call

; class-level inheritance
(class_declaration
  name: (type_identifier) @heritage.class
  (class_heritage
    (extends_clause
      value: (identifier) @heritage.extends))) @heritage

; interface conformance via implements clause
(class_declaration
  name: (type_identifier) @heritage.class
  (class_heritage
    (implements_clause
      (type_identifier) @heritage.implements))) @heritage.impl
`;

/* ── JavaScript / JSX ───────────────────────────────────── */

const jsQueries = `
(class_declaration
  name: (identifier) @name) @definition.class

(function_declaration
  name: (identifier) @name) @definition.function

(method_definition
  name: (property_identifier) @name) @definition.method

(lexical_declaration
  (variable_declarator
    name: (identifier) @name
    value: (arrow_function))) @definition.function

(lexical_declaration
  (variable_declarator
    name: (identifier) @name
    value: (function_expression))) @definition.function

(export_statement
  declaration: (lexical_declaration
    (variable_declarator
      name: (identifier) @name
      value: (arrow_function)))) @definition.function

(export_statement
  declaration: (lexical_declaration
    (variable_declarator
      name: (identifier) @name
      value: (function_expression)))) @definition.function

; Exported const/let with any value (non-function values captured as const)
(export_statement
  declaration: (lexical_declaration
    (variable_declarator
      name: (identifier) @name
      value: (_)))) @definition.const

; Top-level const/let at module scope
(program
  (lexical_declaration
    (variable_declarator
      name: (identifier) @name
      value: (_)))) @definition.const

(import_statement
  source: (string) @import.source) @import

(call_expression
  function: (identifier) @call.name) @call

(call_expression
  function: (member_expression
    property: (property_identifier) @call.name)) @call

; JS class hierarchy (flat AST with identifier child)
(class_declaration
  name: (identifier) @heritage.class
  (class_heritage
    (identifier) @heritage.extends)) @heritage
`;

/* ── Python ─────────────────────────────────────────────── */

const pyQueries = `
(class_definition
  name: (identifier) @name) @definition.class

(function_definition
  name: (identifier) @name) @definition.function

(import_statement
  name: (dotted_name) @import.source) @import

(import_from_statement
  module_name: (dotted_name) @import.source) @import

(call
  function: (identifier) @call.name) @call

(call
  function: (attribute
    attribute: (identifier) @call.name)) @call

; base classes appear inside argument_list
(class_definition
  name: (identifier) @heritage.class
  superclasses: (argument_list
    (identifier) @heritage.extends)) @heritage
`;

/* ── Java ───────────────────────────────────────────────── */

const javaQueries = `
; type-level declarations
(class_declaration name: (identifier) @name) @definition.class
(interface_declaration name: (identifier) @name) @definition.interface
(enum_declaration name: (identifier) @name) @definition.enum
(annotation_type_declaration name: (identifier) @name) @definition.annotation

; callable members
(method_declaration name: (identifier) @name) @definition.method
(constructor_declaration name: (identifier) @name) @definition.constructor

; dependency imports
(import_declaration (_) @import.source) @import

; invocations
(method_invocation name: (identifier) @call.name) @call
(method_invocation object: (_) name: (identifier) @call.name) @call

; superclass
(class_declaration name: (identifier) @heritage.class
  (superclass (type_identifier) @heritage.extends)) @heritage

; implemented interfaces
(class_declaration name: (identifier) @heritage.class
  (super_interfaces (type_list (type_identifier) @heritage.implements))) @heritage.impl
`;

/* ── C ──────────────────────────────────────────────────── */

const cQueries = `
; functions (definitions + forward declarations)
(function_definition declarator: (function_declarator declarator: (identifier) @name)) @definition.function
(declaration declarator: (function_declarator declarator: (identifier) @name)) @definition.function

; aggregate types
(struct_specifier name: (type_identifier) @name) @definition.struct
(union_specifier name: (type_identifier) @name) @definition.union
(enum_specifier name: (type_identifier) @name) @definition.enum
(type_definition declarator: (type_identifier) @name) @definition.typedef

; preprocessor
(preproc_function_def name: (identifier) @name) @definition.macro
(preproc_def name: (identifier) @name) @definition.macro

; includes
(preproc_include path: (_) @import.source) @import

; calls
(call_expression function: (identifier) @call.name) @call
(call_expression function: (field_expression field: (field_identifier) @call.name)) @call
`;

/* ── Go ─────────────────────────────────────────────────── */

const goQueries = `
; function / method
(function_declaration name: (identifier) @name) @definition.function
(method_declaration name: (field_identifier) @name) @definition.method

; type specs
(type_declaration (type_spec name: (type_identifier) @name type: (struct_type))) @definition.struct
(type_declaration (type_spec name: (type_identifier) @name type: (interface_type))) @definition.interface
(type_declaration (type_spec name: (type_identifier) @name)) @definition.type

; imports
(import_declaration (import_spec path: (interpreted_string_literal) @import.source)) @import
(import_declaration (import_spec_list (import_spec path: (interpreted_string_literal) @import.source))) @import

; calls
(call_expression function: (identifier) @call.name) @call
(call_expression function: (selector_expression field: (field_identifier) @call.name)) @call
`;

/* ── C++ ────────────────────────────────────────────────── */

const cppQueries = `
; type declarations
(class_specifier name: (type_identifier) @name) @definition.class
(struct_specifier name: (type_identifier) @name) @definition.struct
(namespace_definition name: (namespace_identifier) @name) @definition.namespace
(enum_specifier name: (type_identifier) @name) @definition.enum

; free and qualified functions
(function_definition declarator: (function_declarator declarator: (identifier) @name)) @definition.function
(function_definition declarator: (function_declarator declarator: (qualified_identifier name: (identifier) @name))) @definition.method

; templates wrapping classes or functions
(template_declaration (class_specifier name: (type_identifier) @name)) @definition.template
(template_declaration (function_definition declarator: (function_declarator declarator: (identifier) @name))) @definition.template

; includes
(preproc_include path: (_) @import.source) @import

; calls (plain, member, qualified, template)
(call_expression function: (identifier) @call.name) @call
(call_expression function: (field_expression field: (field_identifier) @call.name)) @call
(call_expression function: (qualified_identifier name: (identifier) @call.name)) @call
(call_expression function: (template_function name: (identifier) @call.name)) @call

; base-class edges
(class_specifier name: (type_identifier) @heritage.class
  (base_class_clause (type_identifier) @heritage.extends)) @heritage
(class_specifier name: (type_identifier) @heritage.class
  (base_class_clause (access_specifier) (type_identifier) @heritage.extends)) @heritage
`;

/* ── C# ─────────────────────────────────────────────────── */

const csQueries = `
; type declarations
(class_declaration name: (identifier) @name) @definition.class
(interface_declaration name: (identifier) @name) @definition.interface
(struct_declaration name: (identifier) @name) @definition.struct
(enum_declaration name: (identifier) @name) @definition.enum
(record_declaration name: (identifier) @name) @definition.record
(delegate_declaration name: (identifier) @name) @definition.delegate

; namespace scoping
(namespace_declaration name: (identifier) @name) @definition.namespace
(namespace_declaration name: (qualified_name) @name) @definition.namespace

; callable + property members
(method_declaration name: (identifier) @name) @definition.method
(local_function_statement name: (identifier) @name) @definition.function
(constructor_declaration name: (identifier) @name) @definition.constructor
(property_declaration name: (identifier) @name) @definition.property

; using directives
(using_directive (qualified_name) @import.source) @import
(using_directive (identifier) @import.source) @import

; invocations
(invocation_expression function: (identifier) @call.name) @call
(invocation_expression function: (member_access_expression name: (identifier) @call.name)) @call

; base-type edges
(class_declaration name: (identifier) @heritage.class
  (base_list (simple_base_type (identifier) @heritage.extends))) @heritage
(class_declaration name: (identifier) @heritage.class
  (base_list (simple_base_type (generic_name (identifier) @heritage.extends)))) @heritage
`;

/* ── Rust ───────────────────────────────────────────────── */

const rsQueries = `
; item definitions
(function_item name: (identifier) @name) @definition.function
(struct_item name: (type_identifier) @name) @definition.struct
(enum_item name: (type_identifier) @name) @definition.enum
(trait_item name: (type_identifier) @name) @definition.trait
(impl_item type: (type_identifier) @name) @definition.impl
(mod_item name: (identifier) @name) @definition.module

; named constants, statics, type aliases, macros
(type_item name: (type_identifier) @name) @definition.type
(const_item name: (identifier) @name) @definition.const
(static_item name: (identifier) @name) @definition.static
(macro_definition name: (identifier) @name) @definition.macro

; use paths
(use_declaration argument: (_) @import.source) @import

; calls (plain, field, scoped, generic)
(call_expression function: (identifier) @call.name) @call
(call_expression function: (field_expression field: (field_identifier) @call.name)) @call
(call_expression function: (scoped_identifier name: (identifier) @call.name)) @call
(call_expression function: (generic_function function: (identifier) @call.name)) @call

; trait impl relationships
(impl_item trait: (type_identifier) @heritage.trait type: (type_identifier) @heritage.class) @heritage
(impl_item trait: (generic_type type: (type_identifier) @heritage.trait) type: (type_identifier) @heritage.class) @heritage
`;

/* ── Swift ──────────────────────────────────────────────── */

const swiftQueries = `
; class_declaration covers class, struct, enum, and actor in this grammar
(class_declaration "class" name: (type_identifier) @name) @definition.class
(class_declaration "struct" name: (type_identifier) @name) @definition.struct
(class_declaration "enum" name: (type_identifier) @name) @definition.enum
(class_declaration "actor" name: (type_identifier) @name) @definition.class

; protocols
(protocol_declaration
  name: (type_identifier) @name) @definition.interface

; functions (top-level and methods)
(function_declaration
  name: (simple_identifier) @name) @definition.function

; properties
(property_declaration
  (pattern (simple_identifier) @name)) @definition.property

; imports
(import_declaration (identifier) @import.source) @import

; calls
(call_expression
  (simple_identifier) @call.name) @call

(call_expression
  (navigation_expression
    (navigation_suffix
      (simple_identifier) @call.name))) @call
`;

/* ── Dispatch table ─────────────────────────────────────── */

export const LANGUAGE_QUERIES: Record<SupportedLanguages, string> = {
  [SupportedLanguages.TypeScript]: tsQueries,
  [SupportedLanguages.JavaScript]: jsQueries,
  [SupportedLanguages.Python]: pyQueries,
  [SupportedLanguages.Java]: javaQueries,
  [SupportedLanguages.C]: cQueries,
  [SupportedLanguages.Go]: goQueries,
  [SupportedLanguages.CPlusPlus]: cppQueries,
  [SupportedLanguages.CSharp]: csQueries,
  [SupportedLanguages.Rust]: rsQueries,
  [SupportedLanguages.Swift]: swiftQueries,
};
