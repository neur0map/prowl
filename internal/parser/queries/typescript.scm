;; Classes
(class_declaration
  name: (type_identifier) @name) @definition.class

;; Interfaces
(interface_declaration
  name: (type_identifier) @name) @definition.interface

;; Named function declarations
(function_declaration
  name: (identifier) @name) @definition.function

;; Methods
(method_definition
  name: (property_identifier) @name) @definition.method

;; Arrow functions assigned to variables
(lexical_declaration
  (variable_declarator
    name: (identifier) @name
    value: (arrow_function))) @definition.function

;; Function expressions assigned to variables
(lexical_declaration
  (variable_declarator
    name: (identifier) @name
    value: (function_expression))) @definition.function

;; Exported arrow functions
(export_statement
  declaration: (lexical_declaration
    (variable_declarator
      name: (identifier) @name
      value: (arrow_function)))) @definition.function

;; Exported function expressions
(export_statement
  declaration: (lexical_declaration
    (variable_declarator
      name: (identifier) @name
      value: (function_expression)))) @definition.function

;; Exported const/let
(export_statement
  declaration: (lexical_declaration
    (variable_declarator
      name: (identifier) @name
      value: (_)))) @definition.const

;; Top-level const/let
(program
  (lexical_declaration
    (variable_declarator
      name: (identifier) @name
      value: (_)))) @definition.const

;; Import statements
(import_statement
  source: (string) @import.source) @import

;; === Call captures (Phase 4) ===

;; Plain function calls
(call_expression
  function: (identifier) @call.name) @call

;; Member expression calls (obj.method())
(call_expression
  function: (member_expression
    property: (property_identifier) @call.name)) @call

;; === Heritage captures (Phase 5) ===

;; Class extends
(class_declaration
  name: (type_identifier) @heritage.class
  (class_heritage
    (extends_clause
      value: (identifier) @heritage.extends))) @heritage

;; Class implements
(class_declaration
  name: (type_identifier) @heritage.class
  (class_heritage
    (implements_clause
      (type_identifier) @heritage.implements))) @heritage.impl
