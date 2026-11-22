// Port of internal/objectstore/sigv4_test.go (AWS SigV4 suite and S3 docs vectors).
import { describe, expect, it } from "vitest";
import { awsUriEncode, EMPTY_PAYLOAD_HASH, SigV4 } from "./presign";
import { snapshotRunId, validateKey } from "./rundata";

const suite = { accessKeyId: "AKIDEXAMPLE", secretAccessKey: "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY" };
const s3doc = { accessKeyId: "AKIAIOSFODNN7EXAMPLE", secretAccessKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY" };
const suiteTime = new Date(Date.UTC(2015, 7, 30, 12, 36, 0));
const s3Time = new Date(Date.UTC(2013, 4, 24, 0, 0, 0));

describe("SigV4", () => {
  const cases = [
    {
      name: "aws suite get-vanilla",
      s: new SigV4(suite, "us-east-1", "service"),
      method: "GET",
      url: "https://example.amazonaws.com/",
      at: suiteTime,
      want: "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/service/aws4_request, SignedHeaders=host;x-amz-date, Signature=5fa00fa31553b73ebf1942676e86291e8372ff2a2260956d9b8aae1d763fbf31",
    },
    {
      name: "aws suite post-vanilla",
      s: new SigV4(suite, "us-east-1", "service"),
      method: "POST",
      url: "https://example.amazonaws.com/",
      at: suiteTime,
      want: "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/service/aws4_request, SignedHeaders=host;x-amz-date, Signature=5da7c1a2acd57cee7505fc6676e4e544621c30862966e37dddb68e92efbe5d6b",
    },
    {
      name: "aws suite get-vanilla-query-order-key-case",
      s: new SigV4(suite, "us-east-1", "service"),
      method: "GET",
      url: "https://example.amazonaws.com/?Param2=value2&Param1=value1",
      at: suiteTime,
      want: "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/service/aws4_request, SignedHeaders=host;x-amz-date, Signature=b97d918cfa904a5beff61c982a1b6f458b799221646efd99d3219ec94cdf2500",
    },
    {
      name: "s3 docs GET object with range",
      s: new SigV4(s3doc, "us-east-1", "s3"),
      method: "GET",
      url: "https://examplebucket.s3.amazonaws.com/test.txt",
      headers: { Range: "bytes=0-9" },
      at: s3Time,
      want: "AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request, SignedHeaders=host;range;x-amz-content-sha256;x-amz-date, Signature=f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41",
    },
    {
      name: "s3 docs list objects",
      s: new SigV4(s3doc, "us-east-1", "s3"),
      method: "GET",
      url: "https://examplebucket.s3.amazonaws.com/?max-keys=2&prefix=J",
      at: s3Time,
      want: "AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request, SignedHeaders=host;x-amz-content-sha256;x-amz-date, Signature=34b48302e7b5fa45bde8084f4b7868a86f0a534bc59db6670ed5711ef69dc6f7",
    },
  ];
  for (const tc of cases) {
    it(tc.name, async () => {
      const h = new Headers(tc.headers ?? {});
      await tc.s.sign(tc.method, new URL(tc.url), h, EMPTY_PAYLOAD_HASH, tc.at);
      expect(h.get("authorization")).toBe(tc.want);
    });
  }

  it("presigns like the S3 query-parameter example", async () => {
    const s = new SigV4(s3doc, "us-east-1", "s3");
    const got = await s.presign("GET", new URL("https://examplebucket.s3.amazonaws.com/test.txt"), undefined, s3Time, 86400);
    expect(got).toContain("X-Amz-Signature=aeeed9bbccd4d02ee5c0109b86d86835f995330da4c265957d157751f604d404");
    for (const p of [
      "X-Amz-Algorithm=AWS4-HMAC-SHA256",
      "X-Amz-Credential=AKIAIOSFODNN7EXAMPLE%2F20130524%2Fus-east-1%2Fs3%2Faws4_request",
      "X-Amz-Date=20130524T000000Z",
      "X-Amz-Expires=86400",
      "X-Amz-SignedHeaders=host",
    ]) {
      expect(got).toContain(p);
    }
  });

  it("encodes URIs like awsURIEncode", () => {
    expect(awsUriEncode("a b+c/ü~._-", true)).toBe("a%20b%2Bc/%C3%BC~._-");
    expect(awsUriEncode("x=y&z", true)).toBe("x%3Dy%26z");
    expect(awsUriEncode("a/b", false)).toBe("a%2Fb");
  });
});

describe("run data keys (objectstore ValidateKey)", () => {
  it("validates keys", () => {
    validateKey("runs/run_1/artifacts/a.txt");
    for (const bad of ["", "/a", "a/", "a//b", "a/../b", "a\\b", "a\u0001b", "x".repeat(1025)]) expect(() => validateKey(bad), bad).toThrow();
    expect(snapshotRunId("snapshots/default/run_1/1/ws/a.tar")).toBe("run_1");
    expect(snapshotRunId("runs/run_1/x")).toBeUndefined();
  });
});
