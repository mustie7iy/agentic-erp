package directory

// SetupIn is setupIn for the tests, which inject failing transactions.
var SetupIn = setupIn

// CheckLogin is checkLogin, for a test of a state no committed catalog
// may hold while other packages' tests run (a privilege role made a
// member of another).
var CheckLogin = checkLogin
