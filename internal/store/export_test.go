package store

// ClaimSQL is the claim statement, for the plan test of assumption A3.
const ClaimSQL = claimSQL

// ListSQL is the list's statement, for its plan test.
func ListSQL(f Filter) (string, []any) { return listSQL(f) }
