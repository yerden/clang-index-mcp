#include "shared.h"

/* TU2 fan-in on hot_callee: same callee reached from multiple TUs.
 * After dedup, only one row for hot_callee in symbols, and two distinct
 * call_edges into it. */
int tu2_caller(int x) {
    int a = shared_hi(x);
    int b = hot_callee(a);
    return a + b;
}

/* Multi-hop chain (longer than a cycle, not the same shape): A→B→C. */
static int leaf(int x) { return x; }
static int mid(int x)  { return leaf(x) + 1; }
int chain_root(int x)  { return mid(x); }

/* Registers tu2_callback in the .cb field of a struct ops_t via a
 * designated initializer, then invokes the dispatcher. Exercises:
 * (1) typedef canonicalization on cb_t* fields, (2) designated-init
 * field-name recovery, (3) get_indirect_call_sites filter by ".cb". */
static int tu2_callback(int x) { return x + 100; }
int tu2_register_and_dispatch(int x) {
    struct ops_t reg = { .cb = tu2_callback };
    return ops_dispatch(&reg, x);
}
