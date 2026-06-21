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

#endif
