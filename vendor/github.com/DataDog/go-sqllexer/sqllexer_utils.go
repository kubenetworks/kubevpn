package sqllexer

import (
	"strings"
	"unicode"
)

type DBMSType string

const (
	// DBMSSQLServer is a MS SQL
	DBMSSQLServer DBMSType = "mssql"
	// DBMSPostgres is a PostgreSQL Server
	DBMSPostgres DBMSType = "postgresql"
	// DBMSMySQL is a MySQL Server
	DBMSMySQL DBMSType = "mysql"
	// DBMSOracle is a Oracle Server
	DBMSOracle DBMSType = "oracle"
	// DBMSSnowflake is a Snowflake Server
	DBMSSnowflake DBMSType = "snowflake"
)

var commands = map[string]bool{
	"SELECT":        true,
	"INSERT":        true,
	"UPDATE":        true,
	"DELETE":        true,
	"CREATE":        true,
	"ALTER":         true,
	"DROP":          true,
	"JOIN":          true,
	"GRANT":         true,
	"REVOKE":        true,
	"COMMIT":        true,
	"BEGIN":         true,
	"TRUNCATE":      true,
	"MERGE":         true,
	"EXECUTE":       true,
	"EXEC":          true,
	"EXPLAIN":       true,
	"STRAIGHT_JOIN": true,
	"USE":           true,
	"CLONE":         true,
}

var tableIndicators = map[string]bool{
	"FROM":          true,
	"JOIN":          true,
	"INTO":          true,
	"UPDATE":        true,
	"TABLE":         true,
	"STRAIGHT_JOIN": true, // MySQL
	"CLONE":         true, // Snowflake
}

var keywords = map[string]bool{
	"SELECT":     true,
	"INSERT":     true,
	"UPDATE":     true,
	"DELETE":     true,
	"CREATE":     true,
	"ALTER":      true,
	"DROP":       true,
	"GRANT":      true,
	"REVOKE":     true,
	"ADD":        true,
	"ALL":        true,
	"AND":        true,
	"ANY":        true,
	"AS":         true,
	"ASC":        true,
	"BEGIN":      true,
	"BETWEEN":    true,
	"BY":         true,
	"CASE":       true,
	"CHECK":      true,
	"COLUMN":     true,
	"COMMIT":     true,
	"CONSTRAINT": true,
	"DATABASE":   true,
	"DECLARE":    true,
	"DEFAULT":    true,
	"DESC":       true,
	"DISTINCT":   true,
	"ELSE":       true,
	"END":        true,
	"EXEC":       true,
	"EXISTS":     true,
	"FOREIGN":    true,
	"FROM":       true,
	"GROUP":      true,
	"HAVING":     true,
	"IN":         true,
	"INDEX":      true,
	"INNER":      true,
	"INTO":       true,
	"IS":         true,
	"JOIN":       true,
	"KEY":        true,
	"LEFT":       true,
	"LIKE":       true,
	"LIMIT":      true,
	"NOT":        true,
	"ON":         true,
	"OR":         true,
	"ORDER":      true,
	"OUTER":      true,
	"PRIMARY":    true,
	"PROCEDURE":  true,
	"REPLACE":    true,
	"RETURNS":    true,
	"RIGHT":      true,
	"ROLLBACK":   true,
	"ROWNUM":     true,
	"SET":        true,
	"SOME":       true,
	"TABLE":      true,
	"TOP":        true,
	"TRUNCATE":   true,
	"UNION":      true,
	"UNIQUE":     true,
	"USE":        true,
	"VALUES":     true,
	"VIEW":       true,
	"WHERE":      true,
	"CUBE":       true,
	"ROLLUP":     true,
	"LITERAL":    true,
	"WINDOW":     true,
	"VACCUM":     true,
	"ANALYZE":    true,
	"ILIKE":      true,
	"USING":      true,
	"ASSERTION":  true,
	"DOMAIN":     true,
	"CLUSTER":    true,
	"COPY":       true,
	"EXPLAIN":    true,
	"PLPGSQL":    true,
	"TRIGGER":    true,
	"TEMPORARY":  true,
	"UNLOGGED":   true,
	"RECURSIVE":  true,
	"RETURNING":  true,
	"OFFSET":     true,
	"OF":         true,
	"SKIP":       true,
}

func isWhitespace(ch rune) bool {
	return ch == ' ' || ch == '\t' || ch == '\n' || ch == '\r'
}

func isDigit(ch rune) bool {
	return '0' <= ch && ch <= '9'
}

func isExpontent(ch rune) bool {
	return ch == 'e' || ch == 'E'
}

func isLeadingSign(ch rune) bool {
	return ch == '+' || ch == '-'
}

func isLetter(ch rune) bool {
	return unicode.IsLetter(ch) || ch == '_'
}

func isAlphaNumeric(ch rune) bool {
	return isLetter(ch) || isDigit(ch)
}

func isDoubleQuote(ch rune) bool {
	return ch == '"'
}

func isSingleQuote(ch rune) bool {
	return ch == '\''
}

func isOperator(ch rune) bool {
	return ch == '+' || ch == '-' || ch == '*' || ch == '/' || ch == '=' || ch == '<' || ch == '>' || ch == '!' || ch == '&' || ch == '|' || ch == '^' || ch == '%' || ch == '~' || ch == '?' || ch == '@' || ch == ':' || ch == '#'
}

func isWildcard(ch rune) bool {
	return ch == '*'
}

func isSingleLineComment(ch rune, nextCh rune) bool {
	return ch == '-' && nextCh == '-'
}

func isMultiLineComment(ch rune, nextCh rune) bool {
	return ch == '/' && nextCh == '*'
}

func isPunctuation(ch rune) bool {
	return ch == '(' || ch == ')' || ch == ',' || ch == ';' || ch == '.' || ch == ':' || ch == '[' || ch == ']' || ch == '{' || ch == '}'
}

func isEOF(ch rune) bool {
	return ch == 0
}

func isCommand(ident string) bool {
	_, ok := commands[ident]
	return ok
}

func isTableIndicator(ident string) bool {
	_, ok := tableIndicators[ident]
	return ok
}

func isSQLKeyword(token *Token) bool {
	if token.Type != IDENT {
		return false
	}
	_, ok := keywords[strings.ToUpper(token.Value)]
	return ok
}

func isProcedure(token *Token) bool {
	if token.Type != IDENT {
		return false
	}
	return strings.ToUpper(token.Value) == "PROCEDURE" || strings.ToUpper(token.Value) == "PROC"
}

func isBoolean(ident string) bool {
	return strings.ToUpper(ident) == "TRUE" || strings.ToUpper(ident) == "FALSE"
}

func isNull(ident string) bool {
	return strings.ToUpper(ident) == "NULL"
}

func replaceDigits(input string, placeholder string) string {
	var builder strings.Builder

	i := 0
	for i < len(input) {
		if isDigit(rune(input[i])) {
			builder.WriteString(placeholder)
			for i < len(input) && isDigit(rune(input[i])) {
				i++
			}
		} else {
			builder.WriteByte(input[i])
			i++
		}
	}

	return builder.String()
}

func trimQuotes(input string, delim string, closingDelim string) string {
	replacer := strings.NewReplacer(delim, "", closingDelim, "")
	return replacer.Replace(input)
}
