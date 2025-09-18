package tcp

// 1500 - IP Header(20B) - TCP Header Size(20B) - AlignUp(protocol Header Size(1B), 10)
const MTUSize = 1500 - 20 - 20 - 10
