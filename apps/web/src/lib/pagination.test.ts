import { describe, expect, it } from "vitest";
import { DEFAULT_PAGE_SIZE, MAX_PAGE_SIZE, parsePageParams } from "./pagination";

describe("parsePageParams", () => {
  it.each([
    { raw: {}, expected: { page: 1, pageSize: DEFAULT_PAGE_SIZE }, name: "empty params fall back to defaults" },
    { raw: { page: "3", page_size: "50" }, expected: { page: 3, pageSize: 50 }, name: "valid params pass through" },
    { raw: { page: "0" }, expected: { page: 1, pageSize: DEFAULT_PAGE_SIZE }, name: "page below 1 clamps to 1" },
    { raw: { page: "-4" }, expected: { page: 1, pageSize: DEFAULT_PAGE_SIZE }, name: "negative page clamps to 1" },
    { raw: { page: "abc" }, expected: { page: 1, pageSize: DEFAULT_PAGE_SIZE }, name: "non-numeric page falls back" },
    {
      raw: { page: "9999999" },
      expected: { page: 1_000_000, pageSize: DEFAULT_PAGE_SIZE },
      name: "page above the procurement MaxPage clamps instead of 400ing",
    },
    {
      raw: { page_size: "150" },
      expected: { page: 1, pageSize: MAX_PAGE_SIZE },
      name: "page_size above 100 clamps to MaxPageSize",
    },
    { raw: { page_size: "0" }, expected: { page: 1, pageSize: DEFAULT_PAGE_SIZE }, name: "page_size below 1 falls back" },
  ])("$name", ({ raw, expected }) => {
    expect(parsePageParams(raw)).toEqual(expected);
  });
});
