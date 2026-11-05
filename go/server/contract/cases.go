package contract

// Scenarios are the contract cases, run by TestContract (contract build
// tag). Each port step adds its cases here in the same PR as the Go code.
var Scenarios = []Scenario{
	{
		Name:  "health GET",
		Steps: []Step{{Method: "GET", Path: "/health"}},
	},
	{
		Name:  "health HEAD",
		Steps: []Step{{Method: "HEAD", Path: "/health"}},
	},
	{
		Name:  "404 page",
		Steps: []Step{{Method: "GET", Path: "/no/such/route"}},
	},
}
