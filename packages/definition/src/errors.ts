// Error adapter: the definition package throws OfError from @openfactory/core.
// Only this file knows where OfError comes from.

import { OfError } from '@openfactory/core';

export { OfError };

/** invalid returns an OfError with code invalid_request. */
export function invalid(message: string): OfError {
  return new OfError('invalid_request', message);
}
