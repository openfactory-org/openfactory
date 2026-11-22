// The REST client for outside APIs lives in @openfactory/core. Plugin authors
// import it from here.
export {
  API_MAX_ERROR_BODY_BYTES,
  API_MAX_RESPONSE_BYTES,
  API_TIMEOUT_MS,
  ApiClient,
  ApiError,
  isNotFound,
  redactUrl,
  statusCode,
  type ApiClientOptions,
  type ApiResponse,
  type AuthorizeRequest,
  type FetchFn,
  type RequestBody,
  type ResponseKind,
} from "@openfactory/core";
