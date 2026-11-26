// A seed of the adapters' import check, read and never built: an entry
// handler's file outside an ops package calls an entry method.
package srvsvc

type service interface{ ListShares() }

func netShareEnum(svc service) { svc.ListShares() }
