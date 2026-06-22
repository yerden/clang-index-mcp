#include "shared.h"

int tu1_caller(int x) {
    int a = shared_hi(x);
    int b = hot_callee(a);
    int c = factorial(b);
    int d = inline_doubled(c);
    return a + b + c + d;
}

static int square(int x) { return x * x; }

int tu1_indirect(int x) {
    /* dispatch via function pointer; callHierarchy should NOT add an
     * edge from tu1_indirect to square. */
    return dispatch(square, x);
}

/* compared-overrides-arg precedence test: square's address appears as
 * an argument to a function call, but it's inside an equality test, so
 * the category should be 'compared', NOT 'arg_to:assert_eq#1'. */
static int assert_eq(int (*expected)(int), int (*actual)(int)) {
    return expected == actual;
}
int tu1_compared(void) {
    int (*p)(int) = 0;
    return assert_eq(square, p) + (p == square);
}
