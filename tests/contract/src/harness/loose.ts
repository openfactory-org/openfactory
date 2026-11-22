// JSON from the target is checked against the schemas at run time; the tests
// then read fields freely. Loose is the one place that allows that.
// eslint-disable-next-line @typescript-eslint/no-explicit-any
export type Loose = any;
