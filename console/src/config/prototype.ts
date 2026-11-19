/**
 * What the Go gateway really does today, by name. The prototype notice is
 * generated from this list, and everything not on it is declared simulated.
 *
 * A positive list, not a list of absences: a list of what's missing is only
 * true if it is complete, and the first version of the notice proved it
 * wasn't (it left out signatures and the audit chain). This list can only be
 * wrong by claiming too little, which is safe. Add an entry in the step that
 * ships the behaviour, phrased to read after "only" in the notice; with
 * nothing listed, the notice says everything is simulated.
 *
 * A constant, not an environment variable, so no build can make the notice
 * claim more than the code does.
 */
export const GATEWAY_IMPLEMENTED: readonly string[] = ["payment creation", "the double-entry ledger"];
