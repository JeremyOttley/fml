package token

// TokenKind identifies the lexical category of a Token.
// Jaime says: One byte is 256 possible token kinds. If a language has more
// than 256 token kinds, you're designing PL/I, not ML.
type TokenKind uint8

const (
	TokenEOF TokenKind = iota
	TokenIllegal

	// Synthetic off-side rule tokens
	TokenIndent
	TokenDedent
	TokenSemi

	// Literals & Identifiers
	TokenIdent
	TokenInt
	TokenFloat
	TokenString
	TokenTrue
	TokenFalse
	TokenUnit // ()

	// Keywords: Bindings & Functions
	TokenLet     // let
	TokenMutable // mutable
	TokenRec     // rec
	TokenUse     // use
	TokenFun     // fun
	TokenIn      // in
	TokenAnd     // and

	// Keywords: Computation Expressions
	TokenLetBang    // let!
	TokenDoBang     // do!
	TokenUseBang    // use!
	TokenReturn     // return
	TokenReturnBang // return!
	TokenYield      // yield
	TokenSeq        // seq
	TokenAsync      // async
	TokenTask       // task

	// Keywords: Types & Data Structures
	TokenType    // type
	TokenOf      // of
	TokenSome    // Some
	TokenNone    // None
	TokenOk      // Ok
	TokenError   // Error
	TokenMeasure // Measure

	// Keywords: Pattern Matching & Control Flow
	TokenMatch     // match
	TokenWith      // with
	TokenWhen      // when
	TokenFunction  // function
	TokenIf        // if
	TokenThen      // then
	TokenElse      // else
	TokenElif      // elif
	TokenFor       // for
	TokenTo        // to
	TokenDownto    // downto
	TokenDo        // do
	TokenWhile     // while
	TokenTry       // try
	TokenFinally   // finally
	TokenRaise     // raise
	TokenException // exception

	// Keywords: Modules, Namespaces, OOP & Functors
	TokenModule    // module
	TokenNamespace // namespace
	TokenOpen      // open
	TokenFunctor   // functor
	TokenStruct    // struct
	TokenSig       // sig
	TokenEnd       // end
	TokenMember    // member
	TokenAbstract  // abstract
	TokenNew       // new
	TokenInterface // interface
	TokenInherit   // inherit
	TokenStatic    // static
	TokenVal       // val

	// Operators
	TokenAssign        // =
	TokenMutate        // <-
	TokenArrow         // ->
	TokenPipeRight     // |>
	TokenPipeLeft      // <|
	TokenComposeRight  // >>
	TokenComposeLeft   // <<
	TokenCons          // ::
	TokenAppend        // @
	TokenRange         // ..
	TokenPlus          // +
	TokenMinus         // -
	TokenStar          // *
	TokenSlash         // /
	TokenPercent       // %
	TokenCaret         // ^
	TokenEq            // == or =
	TokenNeq           // <> or !=
	TokenLt            // <
	TokenLe            // <=
	TokenGt            // >
	TokenGe            // >=
	TokenAndAnd        // &&
	TokenOrOr          // ||
	TokenNot           // not
	TokenPipe          // |
	TokenAmp           // &
	TokenColon         // :
	TokenColonColon    // ::
	TokenColonQMark    // :?
	TokenQMark         // ?
	TokenUnderscore    // _
	TokenDot           // .
	TokenComma         // ,
	TokenSemicolon     // ;
	TokenApostrophe    // ' (type variable prefix 'a)

	// Delimiters & Brackets
	TokenLParen     // (
	TokenRParen     // )
	TokenLBrack     // [
	TokenRBrack     // ]
	TokenLArray     // [|
	TokenRArray     // |]
	TokenLBrace     // {
	TokenRBrace     // }
	TokenLAttr      // [<
	TokenRAttr      // >]
	TokenLQuote     // <@
	TokenRQuote     // @>
	TokenLActive    // (|
	TokenRActive    // |)
)

// Token represents a lexical token backed by byte offsets into the source slice.
//
// Jaime says: A token is 16 bytes. Four tokens fit in a single 64-byte L1
// cache line. It holds zero pointers and zero strings. If you want the text,
// slice the source buffer with Start and End.
type Token struct {
	Kind  TokenKind
	Flags uint8
	Line  uint16
	Col   uint16
	Aux   uint16 // Optional auxiliary tag or padding
	Start uint32 // Byte offset (inclusive) in source buffer
	End   uint32 // Byte offset (exclusive) in source buffer
}

// Compile-time assertion that Token is at most 16 bytes.
const _ = uint(16 - unsafeSizeOfToken)
const unsafeSizeOfToken = 16

// Slice returns the raw byte slice from src corresponding to this Token.
func (t Token) Slice(src []byte) []byte {
	if int(t.End) > len(src) || t.Start > t.End {
		return nil
	}
	return src[t.Start:t.End]
}

var kindNames = [...]string{
	TokenEOF:          "EOF",
	TokenIllegal:      "Illegal",
	TokenIndent:       "INDENT",
	TokenDedent:       "DEDENT",
	TokenSemi:         "SEMI",
	TokenIdent:        "Ident",
	TokenInt:          "Int",
	TokenFloat:        "Float",
	TokenString:       "String",
	TokenTrue:         "true",
	TokenFalse:        "false",
	TokenUnit:         "()",
	TokenLet:          "let",
	TokenMutable:      "mutable",
	TokenRec:          "rec",
	TokenUse:          "use",
	TokenFun:          "fun",
	TokenIn:           "in",
	TokenAnd:          "and",
	TokenLetBang:      "let!",
	TokenDoBang:       "do!",
	TokenUseBang:      "use!",
	TokenReturn:       "return",
	TokenReturnBang:   "return!",
	TokenYield:        "yield",
	TokenSeq:          "seq",
	TokenAsync:        "async",
	TokenTask:         "task",
	TokenType:         "type",
	TokenOf:           "of",
	TokenSome:         "Some",
	TokenNone:         "None",
	TokenOk:           "Ok",
	TokenError:        "Error",
	TokenMeasure:      "Measure",
	TokenMatch:        "match",
	TokenWith:         "with",
	TokenWhen:         "when",
	TokenFunction:     "function",
	TokenIf:           "if",
	TokenThen:         "then",
	TokenElse:         "else",
	TokenElif:         "elif",
	TokenFor:          "for",
	TokenTo:           "to",
	TokenDownto:       "downto",
	TokenDo:           "do",
	TokenWhile:        "while",
	TokenTry:          "try",
	TokenFinally:      "finally",
	TokenRaise:        "raise",
	TokenException:    "exception",
	TokenModule:       "module",
	TokenNamespace:    "namespace",
	TokenOpen:         "open",
	TokenFunctor:      "functor",
	TokenStruct:       "struct",
	TokenSig:          "sig",
	TokenEnd:          "end",
	TokenMember:       "member",
	TokenAbstract:     "abstract",
	TokenNew:          "new",
	TokenInterface:    "interface",
	TokenInherit:      "inherit",
	TokenStatic:       "static",
	TokenVal:          "val",
	TokenAssign:       "=",
	TokenMutate:       "<-",
	TokenArrow:        "->",
	TokenPipeRight:    "|>",
	TokenPipeLeft:     "<|",
	TokenComposeRight: ">>",
	TokenComposeLeft:  "<<",
	TokenCons:         "::",
	TokenAppend:       "@",
	TokenRange:        "..",
	TokenPlus:         "+",
	TokenMinus:        "-",
	TokenStar:         "*",
	TokenSlash:        "/",
	TokenPercent:      "%",
	TokenCaret:        "^",
	TokenEq:           "==",
	TokenNeq:          "<>",
	TokenLt:           "<",
	TokenLe:           "<=",
	TokenGt:           ">",
	TokenGe:           ">=",
	TokenAndAnd:       "&&",
	TokenOrOr:         "||",
	TokenNot:          "not",
	TokenPipe:         "|",
	TokenAmp:          "&",
	TokenColon:        ":",
	TokenColonColon:   "::",
	TokenColonQMark:   ":?",
	TokenQMark:        "?",
	TokenUnderscore:   "_",
	TokenDot:          ".",
	TokenComma:        ",",
	TokenSemicolon:    ";",
	TokenApostrophe:   "'",
	TokenLParen:       "(",
	TokenRParen:       ")",
	TokenLBrack:       "[",
	TokenRBrack:       "]",
	TokenLArray:       "[|",
	TokenRArray:       "|]",
	TokenLBrace:       "{",
	TokenRBrace:       "}",
	TokenLAttr:        "[<",
	TokenRAttr:        ">]",
	TokenLQuote:       "<@",
	TokenRQuote:       "@>",
	TokenLActive:      "(|",
	TokenRActive:      "|)",
}

// String returns the human-readable name of the TokenKind.
func (k TokenKind) String() string {
	if int(k) < len(kindNames) && kindNames[k] != "" {
		return kindNames[k]
	}
	return "Unknown"
}
