#include "shared.h"

int shared_hi(int x) {
    return x + 1;
}

int hot_callee(int x) {
    return x * 2;
}

int factorial(int n) {
    if (n <= 1) return 1;
    return n * factorial(n - 1);
}

int a_calls_b(int x) {
    return b_calls_a(x) + 1;
}

int b_calls_a(int x) {
    if (x <= 0) return 0;
    return a_calls_b(x - 1);
}

int dispatch(op_t fn, int x) {
    return fn(x);
}

/* Reads o->cb through the typedef-spelled field; the indirect call's
 * callee_type comes from the field declaration (`cb_t *`) and must
 * canonicalize to match the address-take recorded on registered
 * callbacks (`int (*)(int)`). */
int ops_dispatch(struct ops_t *o, int x) {
    return o->cb(x);
}
