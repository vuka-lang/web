<script setup lang="ts">
defineProps<{ days: string[]; booked: string[]; onBook?: (day: string) => void }>()
const picked = defineModel<string>('picked')
</script>

<template>
  <ul class="week">
    <li v-for="d in days" :key="d">
      <button :aria-pressed="picked === d" @click="picked = d">{{ d }}</button>
      <button :disabled="booked.includes(d)" @click="onBook?.(d)">{{ booked.includes(d) ? 'booked' : 'book' }}</button>
    </li>
  </ul>
</template>
