import { expect, test } from "vitest";
import { appendKeyed, appendRow, num } from "./live";

test("appendRow adds a row to each column and keeps the last rows", () => {
  let columns = appendRow(undefined, [1, 10], 2);
  expect(columns).toEqual([[1], [10]]);
  columns = appendRow(columns, [2, 20], 2);
  const before = structuredClone(columns);
  const next = appendRow(columns, [3, 30], 2);
  expect(next).toEqual([
    [2, 3],
    [20, 30],
  ]);
  expect(columns).toEqual(before); // input untouched
});

test("appendKeyed keeps only the keys present now", () => {
  const a = appendKeyed(undefined, { x: 1, y: 2 }, 3);
  const b = appendKeyed(a, { x: 3 }, 3);
  expect(b).toEqual({ x: [1, 3] });
});

test("num", () => {
  expect(num(undefined)).toBe(0);
  expect(num(12n)).toBe(12);
});
