package params

// Deriver is an interface, which the value package does not declare.
type Deriver interface{ Derive(target int) Settings }
