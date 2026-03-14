;; Functions
(function_item name: (identifier) @name) @definition.function

;; Methods (inside impl blocks)
(impl_item
  body: (declaration_list
    (function_item name: (identifier) @name) @definition.method))

;; Structs
(struct_item name: (type_identifier) @name) @definition.struct

;; Enums
(enum_item name: (type_identifier) @name) @definition.enum

;; Traits (map to interface)
(trait_item name: (type_identifier) @name) @definition.interface

;; Type aliases
(type_item name: (type_identifier) @name) @definition.type

;; Constants
(const_item name: (identifier) @name) @definition.const

;; Static items
(static_item name: (identifier) @name) @definition.const

;; Imports (use declarations)
(use_declaration argument: (scoped_identifier path: (identifier) @import.source)) @import
(use_declaration argument: (scoped_identifier path: (scoped_identifier) @import.source)) @import
(use_declaration argument: (identifier) @import.source) @import
(use_declaration argument: (use_as_clause path: (scoped_identifier) @import.source)) @import

;; === Call captures (Phase 4) ===

;; Plain function calls: foo()
(call_expression
  function: (identifier) @call.name) @call

;; Method calls: obj.method()
(call_expression
  function: (field_expression
    field: (field_identifier) @call.name)) @call

;; Scoped function calls: Module::func()
(call_expression
  function: (scoped_identifier
    name: (identifier) @call.name)) @call

;; === Heritage captures (Phase 5) ===

;; impl Trait for Type
(impl_item
  trait: (type_identifier) @heritage.implements
  type: (type_identifier) @heritage.class) @heritage.impl
