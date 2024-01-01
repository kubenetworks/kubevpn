package sqllexer

import "strings"

// ObfuscateAndNormalize takes an input SQL string and returns an normalized SQL string with metadata
// This function is a convenience function that combines the Obfuscator and Normalizer in one pass
func ObfuscateAndNormalize(input string, obfuscator *Obfuscator, normalizer *Normalizer, lexerOpts ...lexerOption) (normalizedSQL string, statementMetadata *StatementMetadata, err error) {
	lexer := New(
		input,
		lexerOpts...,
	)

	var normalizedSQLBuilder strings.Builder

	statementMetadata = &StatementMetadata{
		Tables:     []string{},
		Comments:   []string{},
		Commands:   []string{},
		Procedures: []string{},
	}

	var lastToken Token // The last token that is not whitespace or comment
	var groupablePlaceholder groupablePlaceholder

	for {
		token := lexer.Scan()
		if token.Type == EOF {
			break
		}
		token.Value = obfuscator.ObfuscateTokenValue(token, lexerOpts...)
		normalizer.collectMetadata(&token, &lastToken, statementMetadata)
		normalizer.normalizeSQL(&token, &lastToken, &normalizedSQLBuilder, &groupablePlaceholder, lexerOpts...)
	}

	normalizedSQL = normalizedSQLBuilder.String()

	// Dedupe collected metadata
	dedupeStatementMetadata(statementMetadata)

	return normalizer.trimNormalizedSQL(normalizedSQL), statementMetadata, nil
}
