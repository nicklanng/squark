package main

// sieve returns the number of primes p with p < n (sieve of Eratosthenes).
func sieve(n int) int {
	if n < 3 {
		return 0
	}
	composite := make([]bool, n)
	for i := 2; i*i < n; i++ {
		if !composite[i] {
			for j := i * i; j < n; j += i {
				composite[j] = true
			}
		}
	}
	count := 0
	for i := 2; i < n; i++ {
		if !composite[i] {
			count++
		}
	}
	return count
}
