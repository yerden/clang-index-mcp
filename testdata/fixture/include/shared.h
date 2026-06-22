#ifndef FIXTURE_SHARED_H
#define FIXTURE_SHARED_H

/* shared_hi is declared here and defined in shared.c; both TUs include
 * this header to exercise cross-TU USR dedup (architecture §11.1). */
int shared_hi(int x);

/* dispatcher uses a function pointer; this asserts the known
 * callHierarchy gap stays absent — no edge to the indirect callee. */
typedef int (*op_t)(int);
int dispatch(op_t fn, int x);

/* recursion + cycle: factorial calls itself; a_calls_b/b_calls_a form a
 * length-2 cycle. */
int factorial(int n);
int a_calls_b(int x);
int b_calls_a(int x);

/* fan-in target: called by multiple TUs. */
int hot_callee(int x);

/* static inline target: defined in this header, inlined at each call
 * site. clangd's USRs for static inline functions are TU-qualified, so
 * each TU sees its own instance; this exercises whether our extractor
 * still surfaces them as callees. */
static inline int inline_doubled(int x) {
    return x * 2;
}

/* DPDK-style typedef + struct-of-fn-pointers dispatch (architecture
 * §6.5 gap round 2): exercises three fixes at once.
 *  - Typedef canonicalization: address-takes see `int (*)(int)`,
 *    indirect call site sees `cb_t *` from the field's declared type;
 *    both must canonicalize to the same string.
 *  - Designated-init field-name: struct ops_t { cb_t *cb; ... }
 *    registered via `.cb = my_callback` should record
 *    `stored_in:struct ops_t.cb`, NOT `.<init>`.
 *  - get_indirect_call_sites filtering by callee_expr so an agent can
 *    distinguish `<expr>.cb` dispatch from same-typed siblings. */
typedef int (cb_t)(int);
struct ops_t {
    cb_t *cb;
    cb_t *aux;
};
int ops_dispatch(struct ops_t *o, int x);

#endif
