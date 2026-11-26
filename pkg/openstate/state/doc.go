// Package state is open state's contract: the values its views take and
// return, its enumerations, and the lock conflict error. The filesystem
// contract re-exports them to the adapters, so neither it nor they import
// open state's table. It holds no behaviour.
package state
