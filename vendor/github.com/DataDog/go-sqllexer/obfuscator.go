package sqllexer

import (
	"strings"
)

type obfuscatorConfig struct {
	DollarQuotedFunc           bool `json:"dollar_quoted_func"`
	ReplaceDigits              bool `json:"replace_digits"`
	ReplacePositionalParameter bool `json:"replace_positional_parameter"`
	ReplaceBoolean             bool `json:"replace_boolean"`
	ReplaceNull                bool `json:"replace_null"`
}

type obfuscatorOption func(*obfuscatorConfig)

func WithReplaceDigits(replaceDigits bool) obfuscatorOption {
	return func(c *obfuscatorConfig) {
		c.ReplaceDigits = replaceDigits
	}
}

func WithReplacePositionalParameter(replacePositionalParameter bool) obfuscatorOption {
	return func(c *obfuscatorConfig) {
		c.ReplacePositionalParameter = replacePositionalParameter
	}
}

func WithReplaceBoolean(replaceBoolean bool) obfuscatorOption {
	return func(c *obfuscatorConfig) {
		c.ReplaceBoolean = replaceBoolean
	}
}

func WithReplaceNull(replaceNull bool) obfuscatorOption {
	return func(c *obfuscatorConfig) {
		c.ReplaceNull = replaceNull
	}
}

func WithDollarQuotedFunc(dollarQuotedFunc bool) obfuscatorOption {
	return func(c *obfuscatorConfig) {
		c.DollarQuotedFunc = dollarQuotedFunc
	}
}

type Obfuscator struct {
	config *obfuscatorConfig
}

func NewObfuscator(opts ...obfuscatorOption) *Obfuscator {
	obfuscator := &Obfuscator{
		config: &obfuscatorConfig{},
	}

	for _, opt := range opts {
		opt(obfuscator.config)
	}

	return obfuscator
}

const (
	StringPlaceholder = "?"
	NumberPlaceholder = "?"
)

// Obfuscate takes an input SQL string and returns an obfuscated SQL string.
// The obfuscator replaces all literal values with a single placeholder
func (o *Obfuscator) Obfuscate(input string, lexerOpts ...lexerOption) string {
	var obfuscatedSQL strings.Builder

	lexer := New(
		input,
		lexerOpts...,
	)
	for {
		token := lexer.Scan()
		if token.Type == EOF {
			break
		}
		obfuscatedSQL.WriteString(o.ObfuscateTokenValue(token, lexerOpts...))
	}

	return strings.TrimSpace(obfuscatedSQL.String())
}

func (o *Obfuscator) ObfuscateTokenValue(token Token, lexerOpts ...lexerOption) string {
	switch token.Type {
	case NUMBER:
		return NumberPlaceholder
	case DOLLAR_QUOTED_FUNCTION:
		if o.config.DollarQuotedFunc {
			// obfuscate the content of dollar quoted function
			quotedFunc := token.Value[6 : len(token.Value)-6] // remove the $func$ prefix and suffix
			var obfuscatedDollarQuotedFunc strings.Builder
			obfuscatedDollarQuotedFunc.WriteString("$func$")
			obfuscatedDollarQuotedFunc.WriteString(o.Obfuscate(quotedFunc, lexerOpts...))
			obfuscatedDollarQuotedFunc.WriteString("$func$")
			return obfuscatedDollarQuotedFunc.String()
		} else {
			return StringPlaceholder
		}
	case STRING, INCOMPLETE_STRING, DOLLAR_QUOTED_STRING:
		return StringPlaceholder
	case POSITIONAL_PARAMETER:
		if o.config.ReplacePositionalParameter {
			return StringPlaceholder
		} else {
			return token.Value
		}
	case IDENT, QUOTED_IDENT:
		if o.config.ReplaceBoolean && isBoolean(token.Value) {
			return StringPlaceholder
		}
		if o.config.ReplaceNull && isNull(token.Value) {
			return StringPlaceholder
		}

		if o.config.ReplaceDigits {
			return replaceDigits(token.Value, NumberPlaceholder)
		} else {
			return token.Value
		}
	default:
		return token.Value
	}
}
