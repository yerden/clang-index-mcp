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
