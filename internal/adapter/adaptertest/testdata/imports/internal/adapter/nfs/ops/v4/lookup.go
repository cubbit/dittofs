// A seed of the adapters' import check, read and never built: a command file
// that is no entry handler's calls each entry method.
package v4

type service interface {
	Root()
	SecInfo()
	ListShares()
}

func lookup(svc service) {
	svc.Root()
	svc.SecInfo()
	svc.ListShares()
}
