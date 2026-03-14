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
