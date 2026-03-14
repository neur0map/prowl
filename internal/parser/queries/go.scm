;; Functions
(function_declaration name: (identifier) @name) @definition.function

;; Methods
(method_declaration name: (field_identifier) @name) @definition.method

;; Struct types
(type_declaration (type_spec name: (type_identifier) @name type: (struct_type))) @definition.struct

;; Interface types
(type_declaration (type_spec name: (type_identifier) @name type: (interface_type))) @definition.interface

;; Other type declarations
(type_declaration (type_spec name: (type_identifier) @name)) @definition.type

;; Imports
(import_declaration (import_spec path: (interpreted_string_literal) @import.source)) @import
(import_declaration (import_spec_list (import_spec path: (interpreted_string_literal) @import.source))) @import
