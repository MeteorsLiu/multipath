package conn

// 1500 - TCP Header Size(20B) - protocol Header Size(1B) - Reserved Header Size(1B)
const MTUSize = 1500 - 20 - 1 - 1
