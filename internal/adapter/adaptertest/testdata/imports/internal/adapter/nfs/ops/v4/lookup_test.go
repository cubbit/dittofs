// A seed of the adapters' import check, read and never built: a handler test
// reaches a share through the service it sets up.
package v4

func setup(svc service) { svc.Root() }
