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
