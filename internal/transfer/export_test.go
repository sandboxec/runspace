package transfer

// TrailerSize exposes the length of a tar end-of-archive marker to the
// external tests, which cut streams at offsets relative to it.
const TrailerSize = trailerSize
